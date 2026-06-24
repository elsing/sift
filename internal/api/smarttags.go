package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tunable thresholds for the smart-tagging scorer (Phase 2 reads these too — defined
// here alongside recordTagHistory since they're both part of the same subsystem).
const (
	tagHistorySampleDamper = 3.0 // K in the applied/(applied+dismissed+K) ratio — keeps a 1-2 sample match well below any threshold
	autoApplyScore         = 0.75
	suggestScore           = 0.4
	maxSuggestionsPerMail  = 3
)

// recordTagHistory is the single insert path for every tag decision — manual tagging,
// folder rules, and smart-tagging (live, suggested, or scan-inferred) all funnel
// through this so tag_history stays one consistent shape. score is nil for anything
// that isn't a scored smart-tagging decision (manual, folder_rule).
func (s *Store) recordTagHistory(ownerSubject, accountID, messageID, tagID, senderEmail, subject, source, status string, score *float64) error {
	domain := ""
	if at := strings.LastIndex(senderEmail, "@"); at >= 0 {
		domain = senderEmail[at+1:]
	}
	var accountIDArg any
	if accountID != "" {
		accountIDArg = accountID
	}
	_, err := s.db.Exec(`
		INSERT INTO tag_history (id, owner_subject, account_id, message_id, tag_id, sender_email, sender_domain, subject_tokens, source, status, score, resolved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, CASE WHEN $10 != 'suggested' THEN now() END)`,
		randomID(), ownerSubject, accountIDArg, messageID, tagID, senderEmail, domain, tokenizeSubject(subject), source, status, score,
	)
	if err != nil {
		return err
	}
	if status == "applied" {
		// A tag for this exact (message, tag) pair just became real, through whatever
		// path got us here — manual, a folder rule, bulk-applying to a whole folder,
		// accepting a suggestion, anything. Any OTHER still-pending suggestion for that
		// same pair is now just stale: the thing it was asking about already happened.
		// Without this, a suggestion could sit in the pending list forever even after
		// the tag's been applied some other way, looking like it needs another scan to
		// notice — it doesn't, this is the same fact either way.
		s.db.Exec(
			"UPDATE tag_history SET status = 'applied', resolved_at = now() WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested'",
			messageID, tagID,
		)
	}
	return nil
}

var (
	subjectPunctuation = regexp.MustCompile(`[^a-z0-9\s]+`)
	subjectStopwords   = map[string]bool{
		"re": true, "fw": true, "fwd": true, "the": true, "a": true, "an": true,
		"of": true, "to": true, "for": true, "and": true, "or": true, "is": true,
		"in": true, "on": true, "at": true, "your": true, "you": true, "with": true,
		"from": true, "this": true, "that": true, "are": true, "it": true, "be": true,
	}
)

// tokenizeSubject lowercases, strips punctuation, and drops stopwords/short tokens —
// just enough normalization for the subject-overlap signal to mean something, without
// pulling in a real NLP dependency for what's ultimately a coarse heuristic.
func tokenizeSubject(subject string) []string {
	cleaned := subjectPunctuation.ReplaceAllString(strings.ToLower(subject), " ")
	var tokens []string
	for _, tok := range strings.Fields(cleaned) {
		if len(tok) < 3 || subjectStopwords[tok] {
			continue
		}
		tokens = append(tokens, tok)
	}
	return tokens
}

// tagScore is one candidate tag for a mail, with its computed confidence.
type tagScore struct {
	TagID string
	Score float64
}

// ratio is the shared confidence formula: applied/(applied+dismissed+K). It IS the
// sample-size damper — a single applied/0 dismissed match scores 0.25 (well below any
// threshold), while 8 applied/0 dismissed scores 0.73 — so it doubles as "have we seen
// this enough times to trust it" without a separate check.
func ratio(applied, dismissed int) float64 {
	return float64(applied) / (float64(applied) + float64(dismissed) + tagHistorySampleDamper)
}

// senderRatio/domainRatio score a tag purely off how often it's been applied vs
// dismissed for that exact sender/domain, pooled across every account the owner has
// (owner_subject, not account_id — tags sit above individual IMAP accounts).
func (s *Store) senderRatio(ownerSubject, senderEmail, tagID string) float64 {
	return s.historyRatio("sender_email", ownerSubject, senderEmail, tagID)
}

func (s *Store) domainRatio(ownerSubject, senderDomain, tagID string) float64 {
	if senderDomain == "" {
		return 0
	}
	return s.historyRatio("sender_domain", ownerSubject, senderDomain, tagID)
}

func (s *Store) historyRatio(column, ownerSubject, value, tagID string) float64 {
	var applied, dismissed int
	s.db.QueryRow(
		"SELECT count(*) FILTER (WHERE status = 'applied'), count(*) FILTER (WHERE status = 'dismissed') "+
			"FROM tag_history WHERE owner_subject = $1 AND "+column+" = $2 AND tag_id = $3",
		ownerSubject, value, tagID,
	).Scan(&applied, &dismissed)
	return ratio(applied, dismissed)
}

// jaccard is the overlap ratio between two token sets — 0 if either is empty.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, t := range a {
		setA[t] = true
	}
	union := map[string]bool{}
	for t := range setA {
		union[t] = true
	}
	inter := 0
	for _, t := range b {
		union[t] = true
		if setA[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(union))
}

// subjectRatio scores a tag by how often mail with an overlapping subject (Jaccard >
// 0.3 against past tagged subjects, not exact match — subjects are rarely literally
// identical except thread digests) was applied vs dismissed for that tag.
func (s *Store) subjectRatio(ownerSubject string, tokens []string, tagID string) float64 {
	if len(tokens) == 0 {
		return 0
	}
	rows, err := s.db.Query(
		"SELECT subject_tokens, status FROM tag_history WHERE owner_subject = $1 AND tag_id = $2 AND cardinality(subject_tokens) > 0",
		ownerSubject, tagID,
	)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var applied, dismissed int
	for rows.Next() {
		var pastTokens []string
		var status string
		if rows.Scan(&pastTokens, &status) != nil {
			continue
		}
		if jaccard(tokens, pastTokens) > 0.3 {
			if status == "applied" {
				applied++
			} else if status == "dismissed" {
				dismissed++
			}
		}
	}
	return ratio(applied, dismissed)
}

// suppressedTags returns tag IDs that shouldn't be re-suggested for this exact mail —
// dismissing is purely "this tag is wrong for this email", not a verdict on the sender
// (broader, sender-wide suppression turned out to silence legitimate matches too
// readily, and wasn't actually what dismiss was meant to mean — see blockedSenderTags
// for the separate, explicit, opt-in version of that). Unbounded/permanent, not
// time-windowed: redismissing the same mail/tag pairing isn't expected to happen.
func (s *Store) suppressedTags(ownerSubject, messageID string) map[string]bool {
	suppressed := map[string]bool{}
	rows, err := s.db.Query(
		"SELECT tag_id FROM tag_history WHERE owner_subject = $1 AND message_id = $2 AND status = 'dismissed'",
		ownerSubject, messageID,
	)
	if err != nil {
		return suppressed
	}
	defer rows.Close()
	for rows.Next() {
		var tagID string
		if rows.Scan(&tagID) == nil {
			suppressed[tagID] = true
		}
	}
	return suppressed
}

// blockedSenderTags returns tag IDs explicitly blocked for this sender via the
// separate, opt-in "don't suggest this from this sender again" action — the user's
// deliberate choice to go broader than one email, not an automatic side effect of
// dismissing (see suppressedTags).
func (s *Store) blockedSenderTags(ownerSubject, senderEmail string) map[string]bool {
	blocked := map[string]bool{}
	rows, err := s.db.Query(
		"SELECT tag_id FROM tag_sender_blocks WHERE owner_subject = $1 AND sender_email = $2",
		ownerSubject, senderEmail,
	)
	if err != nil {
		return blocked
	}
	defer rows.Close()
	for rows.Next() {
		var tagID string
		if rows.Scan(&tagID) == nil {
			blocked[tagID] = true
		}
	}
	return blocked
}

// scoreTagsForMail returns every tag scoring >= suggestScore for this sender/subject,
// sorted descending, capped at maxSuggestionsPerMail.
func (s *Store) scoreTagsForMail(ownerSubject, messageID, senderEmail, subject string) []tagScore {
	if senderEmail == "" {
		return nil
	}
	domain := ""
	if at := strings.LastIndex(senderEmail, "@"); at >= 0 {
		domain = senderEmail[at+1:]
	}

	// A bare subject-keyword match must never suggest a tag on its own — skip the
	// whole evaluation for a sender with zero history at this owner, by sender or
	// domain alike, rather than running per-tag queries that can only ever return 0.
	var hasHistory bool
	s.db.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM tag_history WHERE owner_subject = $1 AND (sender_email = $2 OR (sender_domain = $3 AND $3 != '')))",
		ownerSubject, senderEmail, domain,
	).Scan(&hasHistory)
	if !hasHistory {
		return nil
	}

	rows, err := s.db.Query("SELECT id FROM tags WHERE owner_subject = $1", ownerSubject)
	if err != nil {
		return nil
	}
	var tagIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			tagIDs = append(tagIDs, id)
		}
	}
	rows.Close()
	if len(tagIDs) == 0 {
		return nil
	}

	suppressed := s.suppressedTags(ownerSubject, messageID)
	blocked := s.blockedSenderTags(ownerSubject, senderEmail)
	tokens := tokenizeSubject(subject)

	var results []tagScore
	for _, tagID := range tagIDs {
		if suppressed[tagID] || blocked[tagID] {
			continue
		}
		senderScore := s.senderRatio(ownerSubject, senderEmail, tagID)
		score := 0.6 * senderScore
		if score < autoApplyScore {
			// Cheap path: sender alone already clears the bar, no need for the more
			// expensive domain/subject queries.
			score = 0.6*senderScore + 0.25*s.domainRatio(ownerSubject, domain, tagID) + 0.15*s.subjectRatio(ownerSubject, tokens, tagID)
		}
		if score >= suggestScore {
			results = append(results, tagScore{TagID: tagID, Score: score})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > maxSuggestionsPerMail {
		results = results[:maxSuggestionsPerMail]
	}
	return results
}

// ownerAutoTagMode defaults to "review" both when no owner_settings row exists yet
// and on any lookup error — the safer default of the two modes.
func (s *Store) ownerAutoTagMode(ownerSubject string) string {
	var mode string
	if err := s.db.QueryRow("SELECT auto_tag_mode FROM owner_settings WHERE owner_subject = $1", ownerSubject).Scan(&mode); err != nil {
		return "review"
	}
	return mode
}

// existingTagIDs returns the tags a message already has — both the live evaluator and
// the scan need this so they never suggest/auto-apply a tag that's already been
// applied (a real complaint: the scan was re-suggesting tags on mail that was already
// tagged, inbox included).
func (s *Store) existingTagIDs(messageID string) map[string]bool {
	existing := map[string]bool{}
	rows, err := s.db.Query("SELECT tag_id FROM message_tags WHERE message_id = $1", messageID)
	if err != nil {
		return existing
	}
	defer rows.Close()
	for rows.Next() {
		var tagID string
		if rows.Scan(&tagID) == nil {
			existing[tagID] = true
		}
	}
	return existing
}

// evaluateNewMailForSmartTags scores each newly-synced mail and, per the owner's
// auto_tag_mode, either auto-applies a confident match (full_auto only, score >=
// autoApplyScore) or queues it for review. Best-effort throughout: a failure scoring
// or logging one mail doesn't fail the sync that's waiting on this, and never has —
// every call here is fire-and-forget by design.
func (s *Store) evaluateNewMailForSmartTags(accountID string, mails []Mail) {
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		return
	}
	mode := s.ownerAutoTagMode(ownerSubject)

	for _, m := range mails {
		if m.MessageID == "" || m.SenderEmail == "" {
			continue
		}
		existing := s.existingTagIDs(m.MessageID)
		for _, c := range s.scoreTagsForMail(ownerSubject, m.MessageID, m.SenderEmail, m.Subject) {
			if existing[c.TagID] {
				continue // already has this tag — nothing to suggest or apply
			}
			score := c.Score
			if mode == "full_auto" && score >= autoApplyScore {
				s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, c.TagID)
				s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "smart_auto", "applied", &score)
			} else {
				s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "smart_suggested", "suggested", &score)
			}
		}
	}
}

// TagHistoryRoutes registers the suggestion review/accept/dismiss endpoints and the
// owner-level auto-tagging settings (mode, auto-move delay).
func (s *Store) TagHistoryRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/tag-history", func(w http.ResponseWriter, r *http.Request) {
		s.handleListTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("GET /api/misplaced-mail", func(w http.ResponseWriter, r *http.Request) {
		s.handleListMisplacedMail(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		s.handleAcceptTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		s.handleDismissTagHistory(w, r, ownerSubject(r))
	})
	// The undo half of full-auto mode — see Auto-tag activity in settings.
	mux.HandleFunc("POST /api/tag-history/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		s.handleUndoTagHistory(w, r, ownerSubject(r))
	})
	// Separate from plain dismiss — the user's explicit choice to also stop this tag
	// being suggested for this sender at all, not something dismiss implies on its own.
	mux.HandleFunc("POST /api/tag-history/{id}/block-sender", func(w http.ResponseWriter, r *http.Request) {
		s.handleBlockSenderTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("GET /api/owner-settings", func(w http.ResponseWriter, r *http.Request) {
		s.handleGetOwnerSettings(w, r, ownerSubject(r))
	})
	mux.HandleFunc("PUT /api/owner-settings", func(w http.ResponseWriter, r *http.Request) {
		s.handleSetOwnerSettings(w, r, ownerSubject(r))
	})
	// GET, not POST — EventSource (used client-side for the SSE progress stream) can
	// only ever issue GET requests, same constraint /api/search already works under.
	mux.HandleFunc("GET /api/accounts/{id}/scan-tags", s.handleScanTags)
	// GET, not POST — same EventSource constraint as scan-tags above.
	mux.HandleFunc("GET /api/accounts/{id}/folders/apply-tag", s.handleApplyTagToFolder)
	mux.HandleFunc("POST /api/auto-move/run", func(w http.ResponseWriter, r *http.Request) {
		s.handleRunAutoMove(w, r, ownerSubject(r))
	})
}

type TagHistoryEntry struct {
	ID          string   `json:"id"`
	MessageID   string   `json:"messageId"`
	MailID      string   `json:"mailId,omitempty"` // best-effort, ephemeral cache id — empty if that mail's no longer cached, so the client knows there's nothing to open
	Folder      string   `json:"folder,omitempty"` // best-effort, from the mails cache — which folder this mail was found/tagged in
	TagID       string   `json:"tagId"`
	TagName     string   `json:"tagName"`
	TagColor    string   `json:"tagColor"`
	SenderEmail string   `json:"senderEmail"`
	Subject     string   `json:"subject"` // best-effort, from the mails cache if that mail's still resident
	Source      string   `json:"source"`
	Status      string   `json:"status"`
	Score       *float64 `json:"score,omitempty"`
	CreatedAt   string   `json:"createdAt"`
}

type MisplacedMail struct {
	MailID            string `json:"mailId"`
	Sender            string `json:"sender"`
	Subject           string `json:"subject"`
	CurrentFolder     string `json:"currentFolder"`
	DestinationFolder string `json:"destinationFolder"`
	TagID             string `json:"tagId"`
	TagName           string `json:"tagName"`
	TagColor          string `json:"tagColor"`
}

// handleListMisplacedMail finds cached mail that's tagged with something that has a
// folder destination assigned (folder_tag_rules, tag-first — see handleSetTagDestination),
// but isn't actually sitting in that folder. autoMoveTaggedMail already owns moving
// mail *out of inbox* on the same delay, so this deliberately excludes inbox mail —
// it's not a second path to the same outcome, just the rest of the mailbox that
// function never looks at: mail that's already filed somewhere, just not the
// somewhere a tag says it belongs. Same delay applies here too (a mail tagged minutes
// ago isn't "misplaced" yet, it just hasn't had a chance to be moved). Purely DB-side
// against whatever's currently cached (no fresh IMAP listing) — same cache the scan
// and regular sync already populate, so coverage improves the more those run.
func (s *Store) handleListMisplacedMail(w http.ResponseWriter, r *http.Request, owner string) {
	// No age delay here, deliberately — that's specific to autoMoveTaggedMail's "give
	// inbox mail a grace period before whisking it away" rationale, which doesn't
	// apply to mail that's already filed somewhere else. A mail sitting in the wrong
	// non-inbox folder is just wrong, regardless of how recently it was tagged —
	// there's no equivalent reason to wait.
	rows, err := s.db.Query(`
		SELECT m.id, m.sender, m.subject, m.folder, dest.folder, t.id, t.name, t.color
		FROM mails m
		JOIN accounts a ON a.id = m.account_id
		JOIN message_tags mt ON mt.message_id = m.message_id
		JOIN tags t ON t.id = mt.tag_id
		JOIN folder_tag_rules dest ON dest.tag_id = t.id AND dest.account_id = m.account_id
		WHERE a.owner_subject = $1
		  AND m.folder != dest.folder
		  AND m.folder != 'INBOX'
		  AND (SELECT count(*) FROM folder_tag_rules f2 WHERE f2.tag_id = t.id AND f2.account_id = m.account_id) = 1
		ORDER BY m.sent_at DESC
		LIMIT 200`,
		owner,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	misplaced := []MisplacedMail{}
	for rows.Next() {
		var m MisplacedMail
		if err := rows.Scan(&m.MailID, &m.Sender, &m.Subject, &m.CurrentFolder, &m.DestinationFolder, &m.TagID, &m.TagName, &m.TagColor); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		misplaced = append(misplaced, m)
	}
	writeJSON(w, misplaced)
}

// handleListTagHistory serves both the pending-suggestions list (status=suggested,
// optionally scoped to one mail via messageId) and the audit/history view (no status
// filter — every source/status, most recent first). Pending suggestions are bounded to
// the last 30 days so old ones don't pile up forever without a cron-based expiry.
func (s *Store) handleListTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	status := r.URL.Query().Get("status")
	source := r.URL.Query().Get("source") // e.g. "smart_auto" for the Auto-tag activity panel
	// The client only ever knows a mail's ephemeral cache id, not its stable
	// Message-ID — resolve the same way handleGetMailTags does, rather than exposing
	// MessageID (deliberately hidden, json:"-") just for this one lookup.
	messageID := ""
	if mailID := r.URL.Query().Get("mailId"); mailID != "" {
		if resolved, err := s.resolveMessageID(mailID); err == nil {
			messageID = resolved
		}
	}

	query := `
		SELECT th.id, th.message_id, th.tag_id, t.name, t.color, th.sender_email,
		       coalesce((SELECT subject FROM mails WHERE message_id = th.message_id LIMIT 1), ''),
		       coalesce((SELECT id FROM mails WHERE message_id = th.message_id LIMIT 1), ''),
		       coalesce((SELECT folder FROM mails WHERE message_id = th.message_id LIMIT 1), ''),
		       th.source, th.status, th.score, th.created_at
		FROM tag_history th
		JOIN tags t ON t.id = th.tag_id
		WHERE th.owner_subject = $1`
	args := []any{owner}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND th.status = $%d", len(args))
	}
	if messageID != "" {
		args = append(args, messageID)
		query += fmt.Sprintf(" AND th.message_id = $%d", len(args))
	}
	if source != "" {
		args = append(args, source)
		query += fmt.Sprintf(" AND th.source = $%d", len(args))
	}
	if status == "suggested" {
		query += " AND th.created_at > now() - interval '30 days'"
	}
	query += " ORDER BY th.created_at DESC LIMIT 200"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	entries := []TagHistoryEntry{}
	for rows.Next() {
		var e TagHistoryEntry
		if err := rows.Scan(&e.ID, &e.MessageID, &e.TagID, &e.TagName, &e.TagColor, &e.SenderEmail, &e.Subject, &e.MailID, &e.Folder, &e.Source, &e.Status, &e.Score, &e.CreatedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		entries = append(entries, e)
	}
	writeJSON(w, entries)
}

// handleAcceptTagHistory applies a pending suggestion's tag and marks it resolved.
// Scoped to the requesting owner so one user can't act on another's suggestion id.
func (s *Store) handleAcceptTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	var messageID, tagID string
	err := s.db.QueryRow(
		"SELECT message_id, tag_id FROM tag_history WHERE id = $1 AND owner_subject = $2 AND status = 'suggested'",
		id, owner,
	).Scan(&messageID, &tagID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", messageID, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.db.Exec("UPDATE tag_history SET status = 'applied', resolved_at = now() WHERE id = $1", id)
	// Check for an immediate move rather than waiting for the next opportunistic
	// sync — but this still goes through the normal rule, not around it: a suggestion
	// that's sat unaccepted for longer than the delay already qualifies and moves
	// right now, but one accepted the same day it was suggested is still "younger
	// than N days" by autoMoveTaggedMail's own check and stays put until it isn't.
	s.autoMoveTaggedMail(owner)
	// Without this, the tag chip (and any grouping it forms) didn't show up in the
	// inbox until something else happened to trigger a refresh — accepting looked
	// like it silently did nothing client-side even though the tag really had been
	// applied server-side.
	s.broadcaster.publish("mail")
	w.WriteHeader(http.StatusNoContent)
}

// handleUndoTagHistory reverses an already-applied auto-tag (full_auto mode's whole
// premise is that confident matches apply immediately with no review step — this is
// the "easily corrected" half of that bargain). Removes the real tag from the mail
// and marks the history row dismissed, the same status a manual dismiss would leave —
// which means it also feeds suppressedTags, so undoing a wrong auto-tag actually
// teaches the scorer not to repeat it, not just a one-time fix.
func (s *Store) handleUndoTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	var messageID, tagID string
	if err := s.db.QueryRow(
		"SELECT message_id, tag_id FROM tag_history WHERE id = $1 AND owner_subject = $2 AND status = 'applied'",
		id, owner,
	).Scan(&messageID, &tagID); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.Exec("DELETE FROM message_tags WHERE message_id = $1 AND tag_id = $2", messageID, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.db.Exec("UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = $1", id)
	s.broadcaster.publish("mail")
	w.WriteHeader(http.StatusNoContent)
}

// handleDismissTagHistory marks a pending suggestion dismissed — feeds suppressedTags
// so this exact mail stops being re-suggested this tag. Purely a verdict on this one
// email, not its sender; see handleBlockSenderTagHistory for the broader, explicit
// opt-in version of that.
func (s *Store) handleDismissTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	res, err := s.db.Exec(
		"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = $1 AND owner_subject = $2 AND status = 'suggested'",
		id, owner,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBlockSenderTagHistory is the broader, explicit-opt-in sibling of dismiss: not
// just "wrong for this email" but "stop suggesting this tag for this sender, period."
// Dismisses the suggestion too (a block implies the immediate one was also wrong).
func (s *Store) handleBlockSenderTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	var tagID, senderEmail string
	if err := s.db.QueryRow(
		"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = $1 AND owner_subject = $2 AND status = 'suggested' RETURNING tag_id, sender_email",
		id, owner,
	).Scan(&tagID, &senderEmail); err != nil {
		http.NotFound(w, r)
		return
	}
	if senderEmail == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.db.Exec(
		"INSERT INTO tag_sender_blocks (owner_subject, sender_email, tag_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
		owner, senderEmail, tagID,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Blocking the sender means every other still-pending suggestion of this exact tag
	// from this exact sender is now moot too — without this they'd keep sitting in the
	// review queue even though they'll never be acted on, which read as "I blocked this
	// but it's still showing me more of the same".
	s.db.Exec(
		"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE owner_subject = $1 AND tag_id = $2 AND sender_email = $3 AND status = 'suggested'",
		owner, tagID, senderEmail,
	)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleGetOwnerSettings(w http.ResponseWriter, r *http.Request, owner string) {
	mode := "review"
	spamMode := "review"
	delay := 3
	imageRetention := 90
	var backfillCompletedAt *time.Time
	s.db.QueryRow(
		"SELECT auto_tag_mode, spam_mode, auto_move_delay_days, image_cache_retention_days, image_backfill_completed_at FROM owner_settings WHERE owner_subject = $1", owner,
	).Scan(&mode, &spamMode, &delay, &imageRetention, &backfillCompletedAt)
	writeJSON(w, map[string]any{
		"autoTagMode": mode, "spamMode": spamMode, "autoMoveDelayDays": delay,
		"imageCacheRetentionDays": imageRetention, "imageBackfillCompletedAt": backfillCompletedAt,
	})
}

func (s *Store) handleSetOwnerSettings(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		AutoTagMode             string `json:"autoTagMode"`
		SpamMode                string `json:"spamMode"`
		AutoMoveDelayDays       int    `json:"autoMoveDelayDays"`
		ImageCacheRetentionDays int    `json:"imageCacheRetentionDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if req.AutoTagMode != "full_auto" && req.AutoTagMode != "review" {
		http.Error(w, "autoTagMode must be full_auto or review", http.StatusBadRequest)
		return
	}
	if req.SpamMode == "" {
		req.SpamMode = s.ownerSpamMode(owner) // PUT only sends autoTagMode today — keep spam_mode untouched rather than resetting it
	}
	if req.SpamMode != "full_auto" && req.SpamMode != "review" {
		http.Error(w, "spamMode must be full_auto or review", http.StatusBadRequest)
		return
	}
	if req.AutoMoveDelayDays < 0 {
		req.AutoMoveDelayDays = 0
	}
	if req.ImageCacheRetentionDays < 1 {
		req.ImageCacheRetentionDays = 1
	}
	if req.ImageCacheRetentionDays > imageBackfillMaxDays {
		req.ImageCacheRetentionDays = imageBackfillMaxDays
	}
	_, err := s.db.Exec(`
		INSERT INTO owner_settings (owner_subject, auto_tag_mode, spam_mode, auto_move_delay_days, image_cache_retention_days) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_subject) DO UPDATE SET auto_tag_mode = excluded.auto_tag_mode, spam_mode = excluded.spam_mode,
			auto_move_delay_days = excluded.auto_move_delay_days, image_cache_retention_days = excluded.image_cache_retention_days`,
		owner, req.AutoTagMode, req.SpamMode, req.AutoMoveDelayDays, req.ImageCacheRetentionDays,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scanFolderLimit is deliberately higher than the live inbox page size (pageSize) —
// this is meant to capture enough of a folder's real history for the
// folder-concentration signal to mean something, not just the most recent page.
const scanFolderLimit = 200

type scanSummary struct {
	Applied          int               `json:"applied"`
	Suggested        int               `json:"suggested"`
	AlreadyTagged    int               `json:"alreadyTagged"`
	NewTagCandidates []newTagCandidate `json:"newTagCandidates"`
}

type newTagCandidate struct {
	SenderOrDomain string `json:"senderOrDomain"`
	Folder         string `json:"folder"`
	Count          int    `json:"count"`
}

// scanAccountForTags is the one-off bootstrap action: read every folder in this
// account (not just inbox) and use three signals to either apply/suggest tags or
// surface brand-new tag candidates the user has already created by hand-sorting mail
// into folders over time. onProgress is called once per folder processed.
//
// Signal A: mail in a folder with no existing folder_tag_rule is scored against
// existing tag_history exactly like live new mail (same scoreTagsForMail, same
// auto_tag_mode two-tier behavior).
// Signal B: mail in a folder that DOES have a folder_tag_rule is strong retroactive
// evidence for that sender/tag pairing — logged directly as applied, feeding density
// into future scoring immediately rather than waiting for the live cache.
// Signal C: for senders with no tag history at all (Signal A found nothing) who are
// heavily concentrated in one ruleless folder, surface "create a tag for this?" — never
// persisted/auto-applied, since naming a brand-new tag needs the user.
// scope == "inbox" scans just INBOX instead of every folder — the full scan can take
// a while on a large, many-folder mailbox, and not everyone wants to wait through that
// just to bootstrap scoring from their inbox specifically. folders, when non-empty,
// takes precedence over scope entirely — an explicit, user-picked set of folders to
// scan instead of either "everything" or "just inbox".
func (s *Store) scanAccountForTags(ctx context.Context, accountID, scope string, folders []string, onProgress func(done, total int)) (scanSummary, error) {
	summary := scanSummary{}
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return summary, err
	}
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		return summary, err
	}
	mode := s.ownerAutoTagMode(ownerSubject)

	// Clear out this scan's own previous suggestions before generating fresh ones —
	// without this, re-running a scan just kept piling up duplicate "suggested" rows
	// for the exact same (mail, tag) pair every time, rather than replacing last
	// time's findings with this time's. Only ever touches scan_inferred/suggested —
	// never anything already accepted/dismissed, never the live evaluator's own
	// smart_suggested rows, which aren't this scan's to clear.
	if _, err := s.db.Exec(
		"DELETE FROM tag_history WHERE owner_subject = $1 AND source = 'scan_inferred' AND status = 'suggested'",
		ownerSubject,
	); err != nil {
		log.Printf("clear stale scan suggestions: %v", err)
	}

	// Resolved once up front and reused below — both for excluding Trash from "all
	// folders" scans, and for the new-tag-candidate guard further down.
	_, trashFolder, _, _ := detectSpecialUseFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)

	var names []string
	if len(folders) > 0 {
		names = folders // an explicit folder pick is the user's own call — Trash included, if that's what they chose
	} else if scope == "inbox" {
		names = []string{"INBOX"}
	} else {
		names, _, err = listFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
		if err != nil {
			return summary, err
		}
		// "All folders" shouldn't mean Trash too — it's mostly junk/old test mail by
		// nature, and scoring against it polluted suggestions with patterns nobody
		// actually wants learned (a sender's one-off mistake, deleted spam, etc.).
		if trashFolder != "" {
			filtered := names[:0]
			for _, n := range names {
				if n != trashFolder {
					filtered = append(filtered, n)
				}
			}
			names = filtered
		}
	}

	rules := map[string]string{} // folder -> tag id
	if rows, err := s.db.Query("SELECT folder, tag_id FROM folder_tag_rules WHERE account_id = $1", accountID); err == nil {
		for rows.Next() {
			var folder, tagID string
			if rows.Scan(&folder, &tagID) == nil {
				rules[folder] = tagID
			}
		}
		rows.Close()
	}

	knownSenders := map[string]bool{}
	if rows, err := s.db.Query("SELECT DISTINCT sender_email FROM tag_history WHERE owner_subject = $1", ownerSubject); err == nil {
		for rows.Next() {
			var sender string
			if rows.Scan(&sender) == nil {
				knownSenders[sender] = true
			}
		}
		rows.Close()
	}

	type mailInFolder struct {
		mail   Mail
		folder string
	}
	var all []mailInFolder
	err = s.walkAccountFolders(ctx, acct, password, accountID, names, scanFolderLimit, func(m Mail, folder string) {
		all = append(all, mailInFolder{m, folder})
	}, onProgress)
	if err != nil {
		return summary, err
	}

	// Every folder's been fetched, but scoring every mail against tag history (several
	// DB round trips each) for a large mailbox can itself take a noticeable while —
	// without telling the client, the progress bar just sits at "100%" looking stuck
	// for that whole stretch. done = -1 is the client's signal to switch to this phase.
	if onProgress != nil {
		onProgress(-1, len(all))
	}

	// New-tag candidates (Signal C) are about where to file mail going forward — a
	// sender concentrated in one folder years ago isn't a useful predictor of where
	// their mail belongs today, and surfacing it just adds noise to the review queue.
	// Doesn't affect Signal A/B, which score against tags that already exist.
	const newTagCandidateMaxAge = 2 * 365 * 24 * time.Hour
	cutoff := time.Now().Add(-newTagCandidateMaxAge)

	signalCByFolder := map[string][]Mail{} // folder -> ruleless-folder mail with no tag/history match, eligible for "create a tag for this?"
	seenMessageIDs := map[string]bool{}    // a duplicated message (same Message-ID, multiple physical
	// copies across folders/UIDs — see the duplicate-mail incident) should only ever be evaluated
	// once per scan; without this, each extra copy logged its own redundant suggestion for the
	// identical (message, tag) pair, which is what made a single dismissed/blocked suggestion look
	// like it kept "coming back" on the next scan — it wasn't coming back, a sibling copy was scoring fresh.
	for _, mf := range all {
		m, folder := mf.mail, mf.folder
		if m.MessageID == "" {
			continue
		}
		if seenMessageIDs[m.MessageID] {
			continue
		}
		seenMessageIDs[m.MessageID] = true
		if tagID, ok := rules[folder]; ok {
			// recordTagHistory only logs the audit row — it never touches message_tags,
			// the table that actually drives the visible tag chip. Without this insert,
			// mail sitting in a folder with a rule got logged as "applied" (correctly
			// feeding future scoring density) but never genuinely tagged, which read as
			// "the scan found it but didn't actually tag it."
			res, err := s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, tagID)
			// Only log a fresh history row when the tag was actually new — re-running a
			// scan over mail a previous scan (or a live folder-rule move) already tagged
			// otherwise re-logged an identical "applied" row every single time, inflating
			// that sender/tag's count and skewing senderRatio/domainRatio scoring. Same
			// fix as applyFolderTagRule (tags.go) for the same underlying mistake.
			if err == nil {
				if n, err := res.RowsAffected(); err == nil && n > 0 {
					s.recordTagHistory(ownerSubject, accountID, m.MessageID, tagID, m.SenderEmail, m.Subject, "folder_rule", "applied", nil)
					summary.Applied++
				} else {
					summary.AlreadyTagged++
				}
			}
			continue
		}
		if m.SenderEmail == "" {
			continue
		}
		existing := s.existingTagIDs(m.MessageID)
		if len(existing) > 0 {
			// Already tagged (manually, by a folder rule elsewhere, or a previous scan)
			// — re-running the full scorer here only ever costs time without fixing
			// anything: the scan never removes a tag it thinks is wrong, so a second
			// opinion on already-settled mail isn't worth several DB round trips per
			// mail on every single scan. Multi-tag mail that could've picked up one
			// more suggestion is the accepted tradeoff for not re-scoring everything
			// that's already sorted.
			summary.AlreadyTagged++
			continue
		}
		if scored := s.scoreTagsForMail(ownerSubject, m.MessageID, m.SenderEmail, m.Subject); len(scored) > 0 {
			for _, c := range scored {
				score := c.Score
				if mode == "full_auto" && score >= autoApplyScore {
					s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, c.TagID)
					s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "scan_inferred", "applied", &score)
					summary.Applied++
				} else {
					s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "scan_inferred", "suggested", &score)
					summary.Suggested++
				}
			}
			// Scored against existing history at all (even if every match turned out
			// already-applied) still means this sender's "handled" — not eligible for
			// the Signal C new-tag-candidate path below.
			continue
		}
		if knownSenders[m.SenderEmail] {
			continue
		}
		// INBOX is never a sensible new-tag candidate — "this mail is in my inbox" is
		// structural, not a sorting pattern the user created, and accepting one created
		// a tag literally named "INBOX" with a rule mapping INBOX back to itself, which
		// went on to cause real damage (see autoMoveTaggedMail and moveMailToFolder's
		// same-folder guards). Trash is excluded from "all folders" scans entirely
		// above, but this also covers an explicitly-chosen scan that includes it.
		if folder == "INBOX" || (trashFolder != "" && folder == trashFolder) {
			continue
		}
		if t, err := time.Parse(time.RFC3339, m.Date); err == nil && t.Before(cutoff) {
			continue // see newTagCandidateMaxAge above
		}
		signalCByFolder[folder] = append(signalCByFolder[folder], m)
	}

	// One candidate per folder, not per sender — a folder is almost always a single
	// sorting decision regardless of who sent any given mail inside it (that's the
	// whole premise of filing things into folders), so "12 from sender A, 8 from
	// sender B, 4 from sender C, all in Volunteering" is one real signal ("this folder
	// means something"), not three separate near-duplicate suggestions that just
	// fragment the same finding by source address.
	for folder, mails := range signalCByFolder {
		if len(mails) < 3 {
			continue
		}
		senders := map[string]bool{}
		for _, m := range mails {
			senders[m.SenderEmail] = true
		}
		label := fmt.Sprintf("%d different senders", len(senders))
		if len(senders) == 1 {
			for sender := range senders {
				label = sender
			}
		}
		summary.NewTagCandidates = append(summary.NewTagCandidates, newTagCandidate{SenderOrDomain: label, Folder: folder, Count: len(mails)})
	}
	return summary, nil
}

// handleScanTags streams progress over SSE the same way handleSearch does — scanning
// every folder in an account is a multi-IMAP-round-trip operation that can take a
// while, and a blocking response left search in the same position before this pattern
// was adopted there.
func (s *Store) handleScanTags(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	scope := r.URL.Query().Get("scope") // "inbox" or "" (all folders) — ignored if folders is set
	folders := r.URL.Query()["folders"] // optional: scan exactly these folders instead of scope
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sendEvent := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	summary, err := s.scanAccountForTags(r.Context(), accountID, scope, folders, func(done, total int) {
		sendEvent("progress", map[string]int{"done": done, "total": total})
	})
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	if summary.Applied > 0 {
		// full_auto mode can apply tags directly during a scan — same gap as accept:
		// without this, those wouldn't show up in the inbox until something else
		// happened to trigger a refresh.
		s.broadcaster.publish("mail")
	}
	sendEvent("complete", summary)
}

// autoMoveTaggedMail is the inverse of the existing folder_tag_rules use (move into a
// folder -> auto-tag): once a tag's mail has sat unmoved for the owner's configured
// delay, move it into that tag's mapped folder. Reads the same folder_tag_rules table
// the other direction — no new mapping concept for the user to learn.
//
// Mostly opportunistic (called from points already doing work, like a sync) PLUS a
// real periodic tick (watchAutoMove below) — opportunistic alone meant an
// instant_move tag (e.g. Spam, which wants to move right away, not wait for unrelated
// IMAP activity) could sit for an arbitrarily long time in an otherwise-quiet mailbox.
func (s *Store) ownerAutoMoveDelayDays(ownerSubject string) int {
	var delayDays int
	if err := s.db.QueryRow(
		"SELECT auto_move_delay_days FROM owner_settings WHERE owner_subject = $1", ownerSubject,
	).Scan(&delayDays); err != nil {
		return 3 // no settings row yet — same default the column itself has
	}
	return delayDays
}

// movedMailDetail names one piece of mail autoMoveTaggedMail actually moved — kept
// around only so the manual "Move tagged mail now" trigger can show *what* happened
// instead of just a bare count.
type movedMailDetail struct {
	Sender  string `json:"sender"`
	Subject string `json:"subject"`
	Tag     string `json:"tag"`
	Folder  string `json:"folder"`
}

func (s *Store) autoMoveTaggedMail(ownerSubject string) (int, []movedMailDetail) {
	// See autoMoveMu's own comment — this function is called opportunistically from
	// several uncoordinated places, and two overlapping runs racing for the same
	// candidate mail is exactly what made the manual trigger undercount.
	s.autoMoveMu.Lock()
	defer s.autoMoveMu.Unlock()

	delayDays := s.ownerAutoMoveDelayDays(ownerSubject)

	// The delay is meant to be "how long this mail has sat in the inbox", not "how
	// long ago it got tagged" — those are very different clocks for mail tagged well
	// after it arrived (a bulk scan/accept over a backlog tags everything at once,
	// today, regardless of how old the mail itself is). Joining against mails.sent_at
	// here, not tag_history.created_at, is what actually answers "has this mail been
	// sitting here long enough", and incidentally folds in the INBOX-only filter that
	// used to be a separate per-row lookup below.
	// instant_move tags (e.g. "Receipts" — always file it right now, no waiting) skip
	// the global delay entirely rather than just using a smaller number — off by
	// default per-tag, so this only ever kicks in where explicitly turned on.
	rows, err := s.db.Query(`
		SELECT DISTINCT th.message_id, th.tag_id, m.id, coalesce(m.account_id, ''), m.sender, m.subject
		FROM tag_history th
		JOIN mails m ON m.message_id = th.message_id AND m.folder = 'INBOX'
		JOIN tags t ON t.id = th.tag_id
		WHERE th.owner_subject = $1 AND th.status = 'applied'
		  AND coalesce(m.sent_at, th.created_at) < now() - ((CASE WHEN t.instant_move THEN 0 ELSE $2 END) * interval '1 day')
		LIMIT 50`,
		ownerSubject, delayDays,
	)
	if err != nil {
		// Was silently swallowed before — a query bug here (the $2*interval encoding
		// issue this comment used to be a `||` concat that broke parameter type
		// inference) meant auto-move quietly did nothing at all, with no sign anything
		// was wrong short of an unrelated caller surfacing the same query's error.
		log.Printf("auto-move tagged mail: %v", err)
		return 0, nil
	}
	type pair struct{ messageID, tagID, mailID, accountID, sender, subject string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.messageID, &p.tagID, &p.mailID, &p.accountID, &p.sender, &p.subject) == nil && p.accountID != "" {
			pairs = append(pairs, p)
		}
	}
	rows.Close()

	moved := 0
	var details []movedMailDetail
	for _, p := range pairs {
		mailID, accountID := p.mailID, p.accountID

		var folder, tagName string
		if fRows, err := s.db.Query(
			"SELECT f.folder, t.name FROM folder_tag_rules f JOIN tags t ON t.id = f.tag_id WHERE f.account_id = $1 AND f.tag_id = $2", accountID, p.tagID,
		); err == nil {
			count := 0
			for fRows.Next() {
				count++
				fRows.Scan(&folder, &tagName)
			}
			fRows.Close()
			if count != 1 || folder == "INBOX" {
				// no rule, ambiguous (mapped to more than one folder), or — the case that
				// actually bit a real mailbox — a rule mapping a tag back to INBOX itself.
				// This function exists specifically to move mail *out* of inbox; a rule
				// whose destination is INBOX is nonsensical for it and, worse, used to
				// cause a same-folder "move" that silently duplicated the message instead
				// of being a no-op (see moveMailToFolder's own guard for the IMAP-level fix).
				continue
			}
		} else {
			continue
		}

		if err := s.moveMailToFolder(mailID, folder); err != nil {
			continue
		}
		s.db.Exec("DELETE FROM mails WHERE id = $1", mailID) // matches handleMove's own cleanup after a successful move
		moved++
		details = append(details, movedMailDetail{Sender: p.sender, Subject: p.subject, Tag: tagName, Folder: folder})
	}
	if moved > 0 {
		// Some callers (handleAcceptTagHistory, recordManualTagChange) already published
		// their own "mail" event for the tag change itself — a second one here is a
		// harmless no-op refresh for them. The one caller that had NO publish at all
		// until now was the periodic ticker (watchAutoMove): a scheduled move happening
		// in the background, with no interactive action of its own to hang a publish
		// off, left an already-open inbox showing mail that had actually already moved
		// server-side until the next manual refresh.
		s.broadcaster.publish("mail")
	}
	return moved, details
}

// autoMoveCheckInterval mirrors folderCheckInterval's override pattern (accounts.go) —
// AUTO_MOVE_CHECK_INTERVAL_MINUTES mainly for testing without waiting the real default.
func autoMoveCheckInterval() time.Duration {
	if v := os.Getenv("AUTO_MOVE_CHECK_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			return time.Duration(n * float64(time.Minute))
		}
	}
	return 10 * time.Minute
}

// watchAutoMove is the real scheduled half of auto-move (see autoMoveTaggedMail's own
// comment) — runs independently of sync/IMAP activity, so an instant_move tag (Spam)
// actually moves promptly even in an otherwise-quiet mailbox, not just whenever
// something else happens to trigger a sync first.
func (s *Store) watchAutoMove(ctx context.Context) {
	s.autoMoveTaggedMailForEveryOwner() // don't wait a full interval for the first run
	ticker := time.NewTicker(autoMoveCheckInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.autoMoveTaggedMailForEveryOwner()
		}
	}
}

func (s *Store) autoMoveTaggedMailForEveryOwner() {
	rows, err := s.db.Query("SELECT DISTINCT owner_subject FROM accounts")
	if err != nil {
		log.Printf("auto-move: list owners: %v", err)
		return
	}
	var owners []string
	for rows.Next() {
		var owner string
		if rows.Scan(&owner) == nil {
			owners = append(owners, owner)
		}
	}
	rows.Close()
	for _, owner := range owners {
		s.autoMoveTaggedMail(owner)
	}
}

// handleRunAutoMove triggers autoMoveTaggedMail on demand — it normally only runs
// opportunistically (piggybacking on a sync), so without this there was no way to see
// it actually do anything without waiting for new mail to trigger a sync first. Still
// respects auto_move_delay_days; this doesn't move anything that isn't already old
// enough, it just doesn't wait for the next sync to check.
func (s *Store) handleRunAutoMove(w http.ResponseWriter, r *http.Request, owner string) {
	moved, details := s.autoMoveTaggedMail(owner)
	writeJSON(w, map[string]any{"moved": moved, "details": details})
}

// handleApplyTagToFolder backfills a tag onto every mail already sitting in a folder —
// the folder→tag rule (folder_tag_rules) only ever applies going forward, to mail
// moved in *after* the rule is set, so there was no way to retroactively tag what was
// already there short of moving every mail out and back in.
// handleApplyTagToFolder streams progress over SSE (GET, not POST — see the same
// EventSource constraint noted on handleScanTags) and broadcasts a refresh once done,
// so every connected client (including the one that triggered it) picks up the new
// tags without a manual reload.
func (s *Store) handleApplyTagToFolder(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	folder := r.URL.Query().Get("folder")
	tagID := r.URL.Query().Get("tagId")
	if folder == "" || tagID == "" {
		http.Error(w, "folder and tagId are required", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sendEvent := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}

	c, err := dialIMAP(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	defer c.Close()
	mbox, err := c.Select(folder, nil).Wait()
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}

	// "All" means all — page back with the same UID cursor infinite scroll uses, on
	// one held-open connection, until exhausted, not just the most recent batch.
	applied := 0
	var beforeUID uint32
	for page := 1; ; page++ {
		mails, err := fetchFolderMailPage(c, accountID, mbox, folder, scanFolderLimit, beforeUID)
		if err != nil {
			sendEvent("error", map[string]string{"message": err.Error()})
			return
		}
		if len(mails) == 0 {
			break
		}
		for _, m := range mails {
			if m.MessageID == "" {
				continue
			}
			if _, err := s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, tagID); err != nil {
				continue
			}
			s.recordTagHistory(ownerSubject, accountID, m.MessageID, tagID, m.SenderEmail, m.Subject, "folder_rule", "applied", nil)
			applied++
			if m.UID > 0 && (beforeUID == 0 || m.UID < beforeUID) {
				beforeUID = m.UID
			}
		}
		sendEvent("progress", map[string]int{"page": page, "applied": applied})
		if len(mails) < scanFolderLimit {
			break // last (oldest) page was short of a full batch — nothing left before it
		}
	}

	if applied > 0 {
		s.broadcaster.publish("mail") // same signal a new-mail push uses — tells every connected client to refresh
	}
	sendEvent("complete", map[string]int{"applied": applied})
}
