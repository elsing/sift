package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/emersion/go-imap/v2/imapclient"
)

// Tunable thresholds for the smart-tagging scorer (Phase 2 reads these too — defined
// here alongside recordTagHistory since they're both part of the same subsystem).
const (
	tagHistorySampleDamper = 3.0 // K in the applied/(applied+dismissed+K) ratio — keeps a 1-2 sample match well below any threshold
	autoApplyScore         = 0.75
	suggestScore           = 0.4
	maxSuggestionsPerMail  = 3
	dismissSuppressCount   = 2 // dismissals for the same (sender, tag) pair before we stop re-suggesting it
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

// suppressedTags returns tag IDs that shouldn't be re-suggested for this sender —
// dismissed too many times already for that exact (sender, tag) pair. Unbounded/
// permanent, not time-windowed: "this sender doesn't get this tag" is meant to stick.
func (s *Store) suppressedTags(ownerSubject, senderEmail string) map[string]bool {
	suppressed := map[string]bool{}
	rows, err := s.db.Query(
		"SELECT tag_id FROM tag_history WHERE owner_subject = $1 AND sender_email = $2 AND status = 'dismissed' GROUP BY tag_id HAVING count(*) >= $3",
		ownerSubject, senderEmail, dismissSuppressCount,
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

// scoreTagsForMail returns every tag scoring >= suggestScore for this sender/subject,
// sorted descending, capped at maxSuggestionsPerMail.
func (s *Store) scoreTagsForMail(ownerSubject, senderEmail, subject string) []tagScore {
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

	suppressed := s.suppressedTags(ownerSubject, senderEmail)
	tokens := tokenizeSubject(subject)

	var results []tagScore
	for _, tagID := range tagIDs {
		if suppressed[tagID] {
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
		for _, c := range s.scoreTagsForMail(ownerSubject, m.SenderEmail, m.Subject) {
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
	mux.HandleFunc("POST /api/tag-history/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		s.handleAcceptTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		s.handleDismissTagHistory(w, r, ownerSubject(r))
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

// handleListTagHistory serves both the pending-suggestions list (status=suggested,
// optionally scoped to one mail via messageId) and the audit/history view (no status
// filter — every source/status, most recent first). Pending suggestions are bounded to
// the last 30 days so old ones don't pile up forever without a cron-based expiry.
func (s *Store) handleListTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	status := r.URL.Query().Get("status")
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
	w.WriteHeader(http.StatusNoContent)
}

// handleDismissTagHistory marks a pending suggestion dismissed — feeds suppressedTags
// so the same (sender, tag) pairing stops being re-suggested after enough dismissals.
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

func (s *Store) handleGetOwnerSettings(w http.ResponseWriter, r *http.Request, owner string) {
	mode := "review"
	delay := 3
	s.db.QueryRow("SELECT auto_tag_mode, auto_move_delay_days FROM owner_settings WHERE owner_subject = $1", owner).Scan(&mode, &delay)
	writeJSON(w, map[string]any{"autoTagMode": mode, "autoMoveDelayDays": delay})
}

func (s *Store) handleSetOwnerSettings(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		AutoTagMode       string `json:"autoTagMode"`
		AutoMoveDelayDays int    `json:"autoMoveDelayDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if req.AutoTagMode != "full_auto" && req.AutoTagMode != "review" {
		http.Error(w, "autoTagMode must be full_auto or review", http.StatusBadRequest)
		return
	}
	if req.AutoMoveDelayDays < 0 {
		req.AutoMoveDelayDays = 0
	}
	_, err := s.db.Exec(`
		INSERT INTO owner_settings (owner_subject, auto_tag_mode, auto_move_delay_days) VALUES ($1, $2, $3)
		ON CONFLICT (owner_subject) DO UPDATE SET auto_tag_mode = excluded.auto_tag_mode, auto_move_delay_days = excluded.auto_move_delay_days`,
		owner, req.AutoTagMode, req.AutoMoveDelayDays,
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
func (s *Store) scanAccountForTags(accountID string, onProgress func(done, total int)) (scanSummary, error) {
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

	names, _, err := listFolders(acct.Host, acct.Port, acct.Username, password)
	if err != nil {
		return summary, err
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
	for i, folder := range names {
		mails, err := fetchFolderMail(acct, password, folder, scanFolderLimit, 0)
		if err == nil {
			// Without this, scanned mail was never cached locally at all — there was no
			// mails.id to resolve, so a scan-derived suggestion could never be opened
			// from the Smart Tagging panel (the "click into a suggestion" feature
			// silently had nothing to point at for anything the scan found).
			for i := range mails {
				mails[i].AccountID = accountID
			}
			if err := s.upsertMails(accountID, mails); err != nil {
				log.Printf("cache scanned mail %s/%s: %v", acct.Email, folder, err)
			}
			for _, m := range mails {
				all = append(all, mailInFolder{m, folder})
			}
		} // unselectable/empty folder — skip, not fatal to the rest of the scan
		if onProgress != nil {
			onProgress(i+1, len(names))
		}
	}

	senderFolderCount := map[string]map[string]int{}
	senderTotal := map[string]int{}
	for _, mf := range all {
		if mf.mail.SenderEmail == "" {
			continue
		}
		if senderFolderCount[mf.mail.SenderEmail] == nil {
			senderFolderCount[mf.mail.SenderEmail] = map[string]int{}
		}
		senderFolderCount[mf.mail.SenderEmail][mf.folder]++
		senderTotal[mf.mail.SenderEmail]++
	}

	candidates := map[string]newTagCandidate{} // keyed by "sender|folder", dedupes across mail
	for _, mf := range all {
		m, folder := mf.mail, mf.folder
		if m.MessageID == "" {
			continue
		}
		if tagID, ok := rules[folder]; ok {
			s.recordTagHistory(ownerSubject, accountID, m.MessageID, tagID, m.SenderEmail, m.Subject, "folder_rule", "applied", nil)
			summary.Applied++
			continue
		}
		if m.SenderEmail == "" {
			continue
		}
		if scored := s.scoreTagsForMail(ownerSubject, m.SenderEmail, m.Subject); len(scored) > 0 {
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
			continue
		}
		if knownSenders[m.SenderEmail] {
			continue
		}
		total := senderTotal[m.SenderEmail]
		count := senderFolderCount[m.SenderEmail][folder]
		if total >= 3 && float64(count)/float64(total) >= 0.7 {
			candidates[m.SenderEmail+"|"+folder] = newTagCandidate{SenderOrDomain: m.SenderEmail, Folder: folder, Count: count}
		}
	}
	for _, c := range candidates {
		summary.NewTagCandidates = append(summary.NewTagCandidates, c)
	}
	return summary, nil
}

// handleScanTags streams progress over SSE the same way handleSearch does — scanning
// every folder in an account is a multi-IMAP-round-trip operation that can take a
// while, and a blocking response left search in the same position before this pattern
// was adopted there.
func (s *Store) handleScanTags(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
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

	summary, err := s.scanAccountForTags(accountID, func(done, total int) {
		sendEvent("progress", map[string]int{"done": done, "total": total})
	})
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	sendEvent("complete", summary)
}

// autoMoveTaggedMail is the inverse of the existing folder_tag_rules use (move into a
// folder -> auto-tag): once a tag's mail has sat unmoved for the owner's configured
// delay, move it into that tag's mapped folder. Reads the same folder_tag_rules table
// the other direction — no new mapping concept for the user to learn.
//
// Opportunistic, not a real timer (no cron infra in this app) — called from points
// already doing work (a sync), so this fires the next time something happens for an
// account after the delay elapses, not exactly at the delay's edge. That's an accepted
// tradeoff, documented in docs/smart-tagging.md.
func (s *Store) autoMoveTaggedMail(ownerSubject string) {
	var delayDays int
	if err := s.db.QueryRow(
		"SELECT auto_move_delay_days FROM owner_settings WHERE owner_subject = $1", ownerSubject,
	).Scan(&delayDays); err != nil {
		delayDays = 3 // no settings row yet — same default the column itself has
	}

	rows, err := s.db.Query(`
		SELECT DISTINCT message_id, tag_id FROM tag_history
		WHERE owner_subject = $1 AND status = 'applied'
		  AND created_at < now() - ($2 || ' days')::interval
		LIMIT 50`,
		ownerSubject, delayDays,
	)
	if err != nil {
		return
	}
	type pair struct{ messageID, tagID string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.messageID, &p.tagID) == nil {
			pairs = append(pairs, p)
		}
	}
	rows.Close()

	for _, p := range pairs {
		var mailID, accountID string
		// Only mail still sitting in INBOX is a candidate — already moved, archived, or
		// deleted means there's nothing left to do, and that's the normal/common case
		// for most of these rows, not an error.
		if err := s.db.QueryRow(
			"SELECT id, coalesce(account_id, '') FROM mails WHERE message_id = $1 AND folder = 'INBOX'", p.messageID,
		).Scan(&mailID, &accountID); err != nil || accountID == "" {
			continue
		}

		var folders []string
		if fRows, err := s.db.Query(
			"SELECT folder FROM folder_tag_rules WHERE account_id = $1 AND tag_id = $2", accountID, p.tagID,
		); err == nil {
			for fRows.Next() {
				var f string
				if fRows.Scan(&f) == nil {
					folders = append(folders, f)
				}
			}
			fRows.Close()
		}
		if len(folders) != 1 {
			continue // no rule, or ambiguous (mapped to more than one folder) — don't guess
		}

		if err := s.moveMailToFolder(mailID, folders[0]); err != nil {
			continue
		}
		s.db.Exec("DELETE FROM mails WHERE id = $1", mailID) // matches handleMove's own cleanup after a successful move
	}
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

	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	defer c.Close()
	if err := c.Login(acct.Username, password).Wait(); err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
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
