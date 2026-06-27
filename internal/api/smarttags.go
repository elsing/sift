package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
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
// that isn't a scored smart-tagging decision (manual, folder_rule). bodyTokens is nil
// wherever no body was fetched for this decision (manual, folder_rule, the live
// new-mail path) — only scan-time decisions that already fetched a body populate it,
// which is also exactly what feeds rebuildWordProfiles later.
func (s *Store) recordTagHistory(ownerSubject, accountID, messageID, tagID, senderEmail, subject, source, status string, score *float64, bodyTokens []string) error {
	domain := ""
	if at := strings.LastIndex(senderEmail, "@"); at >= 0 {
		domain = senderEmail[at+1:]
	}
	var accountIDArg any
	if accountID != "" {
		accountIDArg = accountID
	}
	var bodyTokensArg any
	if len(bodyTokens) > 0 {
		bodyTokensArg = bodyTokens
	}
	_, err := s.db.Exec(`
		INSERT INTO tag_history (id, owner_subject, account_id, message_id, tag_id, sender_email, sender_domain, subject_tokens, body_tokens, source, status, score, resolved_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, CASE WHEN $11 != 'suggested' THEN now() END)`,
		randomID(), ownerSubject, accountIDArg, messageID, tagID, senderEmail, domain, tokenizeSubject(subject), bodyTokensArg, source, status, score,
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

// maxBodyTokensPerMail bounds how much of a long mail's body actually feeds a tag's
// word profile — a profile only needs enough words to be a meaningful signature, not
// every word in a 10,000-word newsletter.
const maxBodyTokensPerMail = 300

// tokenizeText is tokenizeSubject/tokenizeBody's shared core — lowercases, strips
// punctuation, and drops stopwords/short tokens, just enough normalization for either
// signal to mean something without pulling in a real NLP dependency. max caps the
// token count (0 = unlimited); a subject is short enough not to need one.
func tokenizeText(text string, max int) []string {
	cleaned := subjectPunctuation.ReplaceAllString(strings.ToLower(text), " ")
	var tokens []string
	for _, tok := range strings.Fields(cleaned) {
		if len(tok) < 3 || subjectStopwords[tok] {
			continue
		}
		tokens = append(tokens, tok)
		if max > 0 && len(tokens) >= max {
			break
		}
	}
	return tokens
}

func tokenizeSubject(subject string) []string { return tokenizeText(subject, 0) }

// tokenizeBody feeds a tag's word profile (rebuildWordProfiles) — the body-text
// counterpart to tokenizeSubject, which only ever feeds subjectRatio.
func tokenizeBody(text string) []string { return tokenizeText(text, maxBodyTokensPerMail) }

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

// looksLikeRelayAddress catches a privacy mail-relay's generated alias address —
// Apple's "Hide My Email", Firefox Relay, DuckDuckGo Email Protection, SimpleLogin,
// Fastmail/Proton's masked addresses, and others all work the same way: every alias
// gets its own synthetic, effectively-random local part, often with the real address's
// "@"/"." spelled out inside it for routing (the literal shape that surfaced this:
// "thedailyjames_at_m_jamessmithacademy_com_<random>@icloud.com"). Neither that address
// nor its domain says anything real about the sender in that case — the domain isn't
// even necessarily the sender's at all, it's the *relay's* (the receiving side's own
// privacy feature, not anything the sender chose) — so this is a shape check on the
// local part, deliberately not a list of known relay services/domains: a list only
// ever covers relays already seen, where this generalizes to the next one too.
func looksLikeRelayAddress(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	local := strings.ToLower(email[:at])
	if strings.Contains(local, "_at_") || strings.Contains(local, "_dot_") {
		return true
	}
	return len(local) > 32 // a real, human-chosen local part is essentially never this long; a generated alias routinely is
}

// senderRatio/domainRatio score a tag purely off how often it's been applied vs
// dismissed for that exact sender/domain, pooled across every account the owner has
// (owner_subject, not account_id — tags sit above individual IMAP accounts). Both
// return 0 outright for a relay-generated address (looksLikeRelayAddress) — there's
// nothing real to pool history against. senderRatio's own per-address history would
// just be "this one never-reused alias, seen once"; domainRatio's domain-wide history
// is exactly what let an unrelated relay alias's spam history leak onto this one (see
// looksLikeRelayAddress's own comment for the real example that surfaced it).
// excludeFolderRule, when true, leaves out tag_history rows whose source is
// 'folder_rule' — a real example that surfaced why: the Spam tag's own folder rule
// (Junk → Spam, ensureSpamFolderRule) applied Spam to a Morning Brew newsletter purely
// because one copy of it once sat in Junk for unrelated reasons, with zero actual spam
// judgment involved. That single artifact then reinforced *every future* Morning Brew
// mail as "previously flagged spam" — a folder a message happens to sit in isn't a
// verdict on whether it's spam, the way an actual accept/dismiss or scored auto-apply
// is. Spam's own reinforcement signal (scoreSpam) sets this true; ordinary tag scoring
// (scoreTagsForMail) leaves it false, since folder-filing is a perfectly legitimate
// signal for what an ordinary tag means.
func (s *Store) senderRatio(ownerSubject, senderEmail, tagID string, excludeFolderRule bool) float64 {
	if looksLikeRelayAddress(senderEmail) {
		return 0
	}
	return s.historyRatio("sender_email", ownerSubject, senderEmail, tagID, excludeFolderRule)
}

// sharedFreemailDomains are ordinary providers used by millions of unrelated senders —
// a domain (as opposed to the exact address) having spam history there means almost
// nothing, the same problem as looksLikeRelayAddress but for a normal-looking address
// that just happens to sit on a huge shared domain (the everyday "someone else's
// gmail.com" case, distinct from a relay alias's address being synthetic too).
var sharedFreemailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true,
	"hotmail.co.uk": true, "live.com": true, "msn.com": true, "yahoo.com": true,
	"yahoo.co.uk": true, "aol.com": true, "icloud.com": true, "me.com": true, "mac.com": true,
	"protonmail.com": true, "proton.me": true, "gmx.com": true, "gmx.net": true,
	"zoho.com": true, "mail.com": true, "yandex.com": true,
}

func (s *Store) domainRatio(ownerSubject, senderDomain, tagID string, excludeFolderRule bool) float64 {
	if senderDomain == "" || sharedFreemailDomains[strings.ToLower(senderDomain)] {
		return 0
	}
	return s.historyRatio("sender_domain", ownerSubject, senderDomain, tagID, excludeFolderRule)
}

func (s *Store) historyRatio(column, ownerSubject, value, tagID string, excludeFolderRule bool) float64 {
	if value == "" {
		// An empty sender/domain isn't "no history", it's "we don't actually know who
		// this is" — usually a one-off bad envelope fetch (a flaky IMAP server
		// returning no From for one message). Querying on "" pools this mail in with
		// every other unrelated mail that's ever hit the same gap, which is its own
		// real bug: a completely unrelated history surfacing on this mail, swinging
		// its score around for reasons that have nothing to do with it.
		return 0
	}
	query := "SELECT count(*) FILTER (WHERE status = 'applied'), count(*) FILTER (WHERE status = 'dismissed') " +
		"FROM tag_history WHERE owner_subject = $1 AND " + column + " = $2 AND tag_id = $3"
	if excludeFolderRule {
		query += " AND source != 'folder_rule'"
	}
	var applied, dismissed int
	s.db.QueryRow(query, ownerSubject, value, tagID).Scan(&applied, &dismissed)
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

// bodyProfileRatio scores how well a mail's own body tokens overlap with a tag's
// precomputed word profile (rebuildWordProfiles) — sum of the profile's weights for
// every distinct token the new mail actually contains, capped at 1. Unlike
// subjectRatio, this is one row read against a single aggregate, not a pairwise scan
// over every past mail — the whole point of building the profile ahead of time.
func (s *Store) bodyProfileRatio(tokens []string, tagID string) float64 {
	if len(tokens) == 0 || tagID == "" {
		return 0
	}
	var raw []byte
	if err := s.db.QueryRow("SELECT words FROM tag_word_profiles WHERE tag_id = $1", tagID).Scan(&raw); err != nil {
		return 0
	}
	var weights map[string]float64
	if json.Unmarshal(raw, &weights) != nil {
		return 0
	}
	seen := map[string]bool{}
	score := 0.0
	for _, t := range tokens {
		if seen[t] {
			continue
		}
		seen[t] = true
		score += weights[t]
	}
	if score > 1 {
		score = 1
	}
	return score
}

func (s *Store) ownerWordProfileWeighting(ownerSubject string) string {
	var weighting string
	if err := s.db.QueryRow("SELECT word_profile_weighting FROM owner_settings WHERE owner_subject = $1", ownerSubject).Scan(&weighting); err != nil {
		return "distinctive"
	}
	if weighting != "distinctive" {
		return "distinctive"
	}
	return weighting
}

// rebuildWordProfiles rebuilds every one of this owner's tags' word→weight profiles
// from scratch, from that tag's own applied tag_history.body_tokens. Called once at the
// end of a scan (scanAccountForTags/scanAccountForSpam) — never from the live per-mail
// path — matching the "this only needs to be done once, and rarely" design: a profile
// is a periodically-rebuilt aggregate, not something recomputed on every scoring call.
// Always rebuilds every tag with any applied+body_tokens history, not just whatever
// this particular scan touched — 'distinctive' weighting needs every tag's word counts
// at once (to know how many tags a word shows up in at all), so there's no cheaper
// "just the touched subset" version of this query anyway.
func (s *Store) rebuildWordProfiles(ownerSubject string) {
	weighting := s.ownerWordProfileWeighting(ownerSubject)

	rows, err := s.db.Query(
		"SELECT tag_id, body_tokens FROM tag_history WHERE owner_subject = $1 AND status = 'applied' AND body_tokens IS NOT NULL",
		ownerSubject,
	)
	if err != nil {
		return
	}
	counts := map[string]map[string]int{} // tag_id -> word -> count
	for rows.Next() {
		var tagID string
		var tokens []string
		if rows.Scan(&tagID, &tokens) != nil {
			continue
		}
		words, ok := counts[tagID]
		if !ok {
			words = map[string]int{}
			counts[tagID] = words
		}
		for _, t := range tokens {
			words[t]++
		}
	}
	rows.Close()
	if len(counts) == 0 {
		return
	}

	// Document frequency per word across tags — only meaningful for 'distinctive'
	// weighting (a word common to every tag is a weak discriminator for any of them);
	// skipped entirely for 'plain', which never reads it.
	docFreq := map[string]int{}
	if weighting == "distinctive" {
		for _, words := range counts {
			for w := range words {
				docFreq[w]++
			}
		}
	}
	totalTags := float64(len(counts))

	for tagID, words := range counts {
		total := 0
		for _, c := range words {
			total += c
		}
		if total == 0 {
			continue
		}
		weights := make(map[string]float64, len(words))
		for w, c := range words {
			weight := float64(c) / float64(total) // this tag's own term frequency
			if weighting == "distinctive" {
				// +1 inside the log so a word present in every tag still gets a small
				// non-zero weight rather than dropping to exactly 0.
				weight *= math.Log(totalTags/float64(docFreq[w]) + 1)
			}
			weights[w] = weight
		}
		b, err := json.Marshal(weights)
		if err != nil {
			continue
		}
		s.db.Exec(
			"INSERT INTO tag_word_profiles (tag_id, words, built_at) VALUES ($1, $2, now()) ON CONFLICT (tag_id) DO UPDATE SET words = excluded.words, built_at = excluded.built_at",
			tagID, b,
		)
	}
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

// scoreTagsForMail returns every tag scoring >= minScore for this sender/subject,
// sorted descending, capped at maxSuggestionsPerMail. bodyTokens is nil from any caller
// that hasn't fetched this mail's body (the live new-mail path, today) — bodyProfileRatio
// just contributes 0 in that case, same as a tag with no profile built yet.
func (s *Store) scoreTagsForMail(ownerSubject, messageID, senderEmail, subject string, bodyTokens []string, minScore float64) []tagScore {
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
	// A relay alias's domain isn't necessarily on sharedFreemailDomains (any relay
	// service with its own dedicated domain wouldn't be) — checking the address shape
	// once here, rather than leaving domainRatio to rely solely on its own fixed list,
	// is what actually generalizes to a relay service that list doesn't know about.
	relayLike := looksLikeRelayAddress(senderEmail)

	var results []tagScore
	for _, tagID := range tagIDs {
		if suppressed[tagID] || blocked[tagID] {
			continue
		}
		senderScore := s.senderRatio(ownerSubject, senderEmail, tagID, false)
		score := 0.6 * senderScore
		if score < autoApplyScore {
			// Cheap path: sender alone already clears the bar, no need for the more
			// expensive domain/subject/body queries.
			domainScore := 0.0
			if !relayLike {
				domainScore = s.domainRatio(ownerSubject, domain, tagID, false)
			}
			subjectScore := s.subjectRatio(ownerSubject, tokens, tagID)
			if len(bodyTokens) > 0 {
				score = 0.5*senderScore + 0.2*domainScore + 0.1*subjectScore + 0.2*s.bodyProfileRatio(bodyTokens, tagID)
			} else {
				// No body available — the live new-mail path (evaluateNewMailForSmartTags)
				// never fetches one, by design, so this runs before notifyNewMail decides
				// whether to push. Scoring this exactly as weak as the body-aware case
				// above, just with the body term zeroed out, would silently make this
				// path strictly weaker than it used to be — a sender/domain match
				// confident enough to auto-apply before body scoring existed could now
				// fall just short here, only to clear the bar moments later once the
				// slower body-aware re-evaluation (prefetchMailImages) runs — by which
				// point a notification for a tag marked "no notify" had already gone
				// out, with no way to recall it. Same weights as before body scoring
				// existed, so this path's own strength doesn't regress just because a
				// signal it can't use got added elsewhere.
				score = 0.6*senderScore + 0.25*domainScore + 0.15*subjectScore
			}
		}
		if score >= minScore {
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

// ownerTagAutoApplyScore returns the confidence threshold above which full-auto mode
// applies a tag without asking. Defaults to the hardcoded constant when unset.
func (s *Store) ownerTagAutoApplyScore(ownerSubject string) float64 {
	var score float64
	if err := s.db.QueryRow("SELECT tag_auto_apply_score FROM owner_settings WHERE owner_subject = $1", ownerSubject).Scan(&score); err != nil {
		return autoApplyScore
	}
	if score < 0.4 || score > 1 {
		return autoApplyScore
	}
	return score
}

// ownerSpamAutoApplyScore returns the confidence threshold above which full-auto mode
// applies the Spam tag without asking. Defaults to the hardcoded constant when unset.
func (s *Store) ownerSpamAutoApplyScore(ownerSubject string) float64 {
	var score float64
	if err := s.db.QueryRow("SELECT spam_auto_apply_score FROM owner_settings WHERE owner_subject = $1", ownerSubject).Scan(&score); err != nil {
		return spamAutoApplyScore
	}
	if score < 0.4 || score > 1 {
		return spamAutoApplyScore
	}
	return score
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

// evaluateOneMailForSmartTags scores a single mail and, per the owner's auto_tag_mode,
// either auto-applies a confident match or queues it for review — the per-mail body of
// evaluateNewMailForSmartTags, factored out so prefetchMailImages (imageproxy.go) can
// run this same scoring again once it has a body in hand. bodyTokens is nil from the
// fast, body-less sync-time pass; the existing/alreadySuggested guards below mean
// running this twice for the same mail never double-logs anything — a second pass with
// a body just gives bodyProfileRatio something to actually contribute.
func (s *Store) evaluateOneMailForSmartTags(ownerSubject, accountID, mode string, m Mail, bodyTokens []string) {
	if m.MessageID == "" || m.SenderEmail == "" {
		return
	}
	existing := s.existingTagIDs(m.MessageID)
	for _, c := range s.scoreTagsForMail(ownerSubject, m.MessageID, m.SenderEmail, m.Subject, bodyTokens, suggestScore) {
		if existing[c.TagID] {
			continue // already has this tag — nothing to suggest or apply
		}
		score := c.Score
		if mode == "full_auto" && score >= s.ownerTagAutoApplyScore(ownerSubject) {
			s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, c.TagID)
			s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "smart_auto", "applied", &score, bodyTokens)
			continue
		}
		// A mail that gets a fresh UID without genuinely being new (moved by auto-move,
		// a folder rule, or anything else outside this sync) can reach this path more
		// than once for the same underlying message — skip if a suggestion for this
		// exact (message, tag) is already pending rather than stacking another.
		var alreadySuggested bool
		s.db.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested')",
			m.MessageID, c.TagID,
		).Scan(&alreadySuggested)
		if alreadySuggested {
			continue
		}
		s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "smart_suggested", "suggested", &score, bodyTokens)
	}
}

// evaluateNewMailForSmartTags scores each newly-synced mail and, per the owner's
// auto_tag_mode, either auto-applies a confident match (full_auto only, score >=
// autoApplyScore) or queues it for review. Best-effort throughout: a failure scoring
// or logging one mail doesn't fail the sync that's waiting on this, and never has —
// every call here is fire-and-forget by design. Runs with no body (syncAccount only
// hands over envelope/flags) — prefetchMailImages' own background pass (imageproxy.go)
// re-evaluates each of these mails again once it has fetched a body, so the heatmap
// signal isn't lost, just delayed by however long that background fetch takes.
func (s *Store) evaluateNewMailForSmartTags(accountID string, mails []Mail) {
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		return
	}
	mode := s.ownerAutoTagMode(ownerSubject)
	for _, m := range mails {
		s.evaluateOneMailForSmartTags(ownerSubject, accountID, mode, m, nil)
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
	// Job-backed (handleOwnerJobSSE, scanjobs.go) — "Move all" used to be one IMAP
	// move request per mail from the client (Promise.all over the whole list); each
	// move is real, possibly-slow IMAP work, easily enough at once to need
	// persistence and progress rather than a client-side fan-out.
	mux.HandleFunc("GET /api/misplaced-mail/move-all", func(w http.ResponseWriter, r *http.Request) {
		s.handleMoveAllMisplacedMail(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/misplaced-mail/move-all/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelOwnerJob(w, r, ownerSubject(r), "move-misplaced-mail")
	})
	mux.HandleFunc("POST /api/tag-history/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		s.handleAcceptTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		s.handleDismissTagHistory(w, r, ownerSubject(r))
	})
	// Not a verdict either way, just "stop showing me this" — unlike dismiss, doesn't
	// feed suppressedTags (so it can resurface on a future scan/sync) and doesn't touch
	// Junk restoration.
	mux.HandleFunc("POST /api/tag-history/{id}/clear", func(w http.ResponseWriter, r *http.Request) {
		s.handleClearTagHistory(w, r, ownerSubject(r))
	})
	// Bulk counterparts of accept/dismiss/clear above — "Accept all"/"Dismiss
	// all"/"Clear list" used to be one HTTP request per suggestion; see
	// bulkTagHistoryIDs' own comment.
	mux.HandleFunc("POST /api/tag-history/bulk-accept", func(w http.ResponseWriter, r *http.Request) {
		s.handleBulkAcceptTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/bulk-dismiss", func(w http.ResponseWriter, r *http.Request) {
		s.handleBulkDismissTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/bulk-clear", func(w http.ResponseWriter, r *http.Request) {
		s.handleBulkClearTagHistory(w, r, ownerSubject(r))
	})
	// The undo half of full-auto mode — see Auto-tag activity in settings.
	mux.HandleFunc("POST /api/tag-history/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		s.handleUndoTagHistory(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tag-history/bulk-undo", func(w http.ResponseWriter, r *http.Request) {
		s.handleBulkUndoTagHistory(w, r, ownerSubject(r))
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
	mux.HandleFunc("GET /api/accounts/{id}/scan-tags", func(w http.ResponseWriter, r *http.Request) {
		s.handleScanTags(w, r, ownerSubject(r))
	})
	// The scan itself now runs on a server-lifetime context (scanjobs.go), not this
	// request's — closing the SSE connection no longer stops it, so cancelling needs
	// its own explicit action.
	mux.HandleFunc("POST /api/accounts/{id}/scan-tags/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelScanJob(w, r, ownerSubject(r), "tags")
	})
	// What a panel reopening after a reload uses to find a still-running scan without
	// already knowing which account it was started against.
	mux.HandleFunc("GET /api/scan-jobs/running", func(w http.ResponseWriter, r *http.Request) {
		s.handleRunningScanJob(w, r, ownerSubject(r))
	})
	// GET, not POST — same EventSource constraint as scan-tags above.
	mux.HandleFunc("GET /api/accounts/{id}/folders/apply-tag", s.handleApplyTagToFolder)
	mux.HandleFunc("POST /api/accounts/{id}/folders/apply-tag/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelScanJob(w, r, ownerSubject(r), "apply-tag-folder")
	})
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
// misplacedMail is the query both handleListMisplacedMail (the review list) and
// moveMisplacedMail (the "Move all" job) share — see handleListMisplacedMail's own
// comment for why INBOX/Sent are excluded and why a tag needs exactly one folder rule
// to qualify.
func (s *Store) misplacedMail(owner string) ([]MisplacedMail, error) {
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
		  AND m.folder NOT ILIKE '%sent%'
		  AND (SELECT count(*) FROM folder_tag_rules f2 WHERE f2.tag_id = t.id AND f2.account_id = m.account_id) = 1
		  AND NOT EXISTS (
		    SELECT 1 FROM message_tags mt2
		    JOIN folder_tag_rules ftr2 ON ftr2.tag_id = mt2.tag_id AND ftr2.account_id = m.account_id
		    WHERE mt2.message_id = m.message_id AND ftr2.folder = m.folder
		  )
		ORDER BY m.sent_at DESC
		LIMIT 200`,
		owner,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	misplaced := []MisplacedMail{}
	for rows.Next() {
		var m MisplacedMail
		if err := rows.Scan(&m.MailID, &m.Sender, &m.Subject, &m.CurrentFolder, &m.DestinationFolder, &m.TagID, &m.TagName, &m.TagColor); err != nil {
			return nil, err
		}
		misplaced = append(misplaced, m)
	}
	return misplaced, nil
}

func (s *Store) handleListMisplacedMail(w http.ResponseWriter, r *http.Request, owner string) {
	// No age delay here, deliberately — that's specific to autoMoveTaggedMail's "give
	// inbox mail a grace period before whisking it away" rationale, which doesn't
	// apply to mail that's already filed somewhere else. A mail sitting in the wrong
	// non-inbox folder is just wrong, regardless of how recently it was tagged —
	// there's no equivalent reason to wait.
	misplaced, err := s.misplacedMail(owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, misplaced)
}

func (s *Store) handleMoveAllMisplacedMail(w http.ResponseWriter, r *http.Request, owner string) {
	s.handleOwnerJobSSE(w, r, owner, "move-misplaced-mail", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
		return s.moveAllMisplacedMail(ctx, owner, onProgress)
	})
}

func (s *Store) moveAllMisplacedMail(ctx context.Context, owner string, onProgress func(done, total int)) (any, error) {
	misplaced, err := s.misplacedMail(owner)
	if err != nil {
		return nil, err
	}
	moved := 0
	for i, m := range misplaced {
		if ctx.Err() != nil {
			break
		}
		if err := s.moveMailToFolder(m.MailID, m.DestinationFolder); err == nil {
			moved++
		}
		if onProgress != nil {
			onProgress(i+1, len(misplaced))
		}
	}
	if moved > 0 {
		s.broadcaster.publish("mail")
	}
	return map[string]int{"moved": moved}, nil
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
	mailID := r.URL.Query().Get("mailId")
	messageID := ""
	if mailID != "" {
		if resolved, err := s.resolveMessageID(mailID); err == nil {
			messageID = resolved
		} else {
			// A mailId was explicitly asked for but doesn't resolve to a real Message-ID
			// (mock/demo mail, most often) — that mail can have no history, full stop.
			// Falling through with the filter silently dropped instead returned every
			// pending suggestion in the whole mailbox, misattributed to this one mail —
			// which is exactly what made a single demo email look like it had 100+
			// duplicated spam suggestions on it.
			writeJSON(w, []TagHistoryEntry{})
			return
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
	// Mail date, not when the row was logged — a scan logs every decision in whatever
	// order it happened to walk folders/messages in, which read as "not sorted by date"
	// when that order doesn't line up with the mail's own sent date. Falls back to
	// th.created_at for anything no longer cached in mails (an old/evicted mail still
	// needs some deterministic order).
	orderBy := " ORDER BY coalesce((SELECT sent_at FROM mails WHERE message_id = th.message_id LIMIT 1), th.created_at) DESC"
	if status == "suggested" {
		query += " AND th.created_at > now() - interval '30 days'"
		// No LIMIT here — the 30-day window above is the only cap. A flat LIMIT 200 used
		// to silently cut off whole senders' worth of suggestions once a scan produced
		// more pending rows than that, before the client ever got a chance to group them.
		query += orderBy
	} else {
		query += orderBy + " LIMIT 500"
	}

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
	// Backgrounded: this can mean a real IMAP move (instant_move tags, e.g. Spam, skip
	// the delay entirely), which made every single accept visibly hang on the actual
	// network round trip — the tag itself is already applied above by this point, so
	// the response doesn't need to wait for the move too.
	go s.autoMoveTaggedMail(owner)
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
	var messageID, tagID, source string
	if err := s.db.QueryRow(
		"SELECT message_id, tag_id, source FROM tag_history WHERE id = $1 AND owner_subject = $2 AND status = 'applied'",
		id, owner,
	).Scan(&messageID, &tagID, &source); err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.Exec("DELETE FROM message_tags WHERE message_id = $1 AND tag_id = $2", messageID, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.db.Exec("UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = $1", id)
	// Backgrounded — its slow path is a live IMAP search across every account on a
	// cache miss, which made undo visibly hang on the network round trip even though
	// the dismiss itself (above) had already gone through. No publish needed from this
	// call specifically — the unconditional one below already covers refreshing for the
	// undo itself, regardless of whether this restore finds anything to do.
	go s.restoreFromJunkIfSpamDeclined(owner, messageID, source)
	s.broadcaster.publish("mail")
	w.WriteHeader(http.StatusNoContent)
}

// handleBulkUndoTagHistory is "Undo all" (Auto-tag activity) — see bulkTagHistoryIDs'
// own comment on why this exists at all (one request instead of one per row).
func (s *Store) handleBulkUndoTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	ids, ok := bulkTagHistoryIDs(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rows, err := s.db.Query(
		"SELECT message_id, tag_id, source FROM tag_history WHERE id = ANY($1) AND owner_subject = $2 AND status = 'applied'",
		ids, owner,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type triple struct{ messageID, tagID, source string }
	var triples []triple
	for rows.Next() {
		var t triple
		if rows.Scan(&t.messageID, &t.tagID, &t.source) == nil {
			triples = append(triples, t)
		}
	}
	rows.Close()
	if len(triples) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	for _, t := range triples {
		if _, err := tx.Exec("DELETE FROM message_tags WHERE message_id = $1 AND tag_id = $2", t.messageID, t.tagID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if _, err := tx.Exec(
		"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = ANY($1) AND owner_subject = $2 AND status = 'applied'",
		ids, owner,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// One goroutine per affected message, same as handleBulkDismissTagHistory.
	for _, t := range triples {
		go s.restoreFromJunkIfSpamDeclined(owner, t.messageID, t.source)
	}
	s.broadcaster.publish("mail")
	w.WriteHeader(http.StatusNoContent)
}

// handleDismissTagHistory marks a pending suggestion dismissed — feeds suppressedTags
// so this exact mail stops being re-suggested this tag. Purely a verdict on this one
// email, not its sender; see handleBlockSenderTagHistory for the broader, explicit
// opt-in version of that.
func (s *Store) handleDismissTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	var messageID, source string
	if err := s.db.QueryRow(
		"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = $1 AND owner_subject = $2 AND status = 'suggested' RETURNING message_id, source",
		id, owner,
	).Scan(&messageID, &source); err != nil {
		http.NotFound(w, r)
		return
	}
	// Backgrounded — see handleUndoTagHistory's identical comment above. The dismiss
	// itself is already durable by the time this returns; restoring from Junk (if it
	// even applies) doesn't need to hold up the response. Unlike undo, plain dismiss
	// has no unconditional publish of its own (it doesn't touch message_tags), so this
	// one does need to publish itself — but only if it actually moved something.
	go func() {
		if s.restoreFromJunkIfSpamDeclined(owner, messageID, source) {
			s.broadcaster.publish("mail")
		}
	}()
	w.WriteHeader(http.StatusNoContent)
}

// handleClearTagHistory removes a pending suggestion from the queue with no judgment
// recorded at all — deleted outright, not marked dismissed. Dismissing is a real
// verdict ("this tag is wrong for this mail") that feeds suppressedTags and can
// restore mail from Junk; clearing is just "I don't want to look at this anymore",
// which means the exact same suggestion is free to resurface on a future scan or sync
// if it still scores the same way, same as if it had never been cleared.
func (s *Store) handleClearTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	if _, err := s.db.Exec(
		"DELETE FROM tag_history WHERE id = $1 AND owner_subject = $2 AND status = 'suggested'",
		id, owner,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bulkTagHistoryIDs decodes the {"ids": [...]} body the three bulk-action handlers
// below all share — "Accept all"/"Dismiss all"/"Clear list" used to mean one HTTP
// request per suggestion (Promise.all over the whole list client-side), which on an
// uncapped pending-suggestions list could mean thousands of round trips for one tap.
// One request with every id, handled in one or two queries server-side instead.
func bulkTagHistoryIDs(r *http.Request) ([]string, bool) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.IDs) == 0 {
		return nil, false
	}
	return req.IDs, true
}

// handleBulkAcceptTagHistory is "Accept all" — see bulkTagHistoryIDs' own comment.
func (s *Store) handleBulkAcceptTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	ids, ok := bulkTagHistoryIDs(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Return immediately — DB writes + IMAP moves are done async. The client gets 204
	// in < 1ms; the "mail" broadcast arrives when the work actually finishes.
	w.WriteHeader(http.StatusNoContent)
	go func() {
		tx, err := s.db.Begin()
		if err != nil {
			log.Printf("bulk-accept begin: %v", err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`
			INSERT INTO message_tags (message_id, tag_id)
			SELECT message_id, tag_id FROM tag_history WHERE id = ANY($1) AND owner_subject = $2 AND status = 'suggested'
			ON CONFLICT DO NOTHING`,
			ids, owner,
		); err != nil {
			log.Printf("bulk-accept insert: %v", err)
			return
		}
		if _, err := tx.Exec(
			"UPDATE tag_history SET status = 'applied', resolved_at = now() WHERE id = ANY($1) AND owner_subject = $2 AND status = 'suggested'",
			ids, owner,
		); err != nil {
			log.Printf("bulk-accept update: %v", err)
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("bulk-accept commit: %v", err)
			return
		}
		s.autoMoveTaggedMail(owner)
		s.broadcaster.publish("mail")
	}()
}

// handleBulkDismissTagHistory is "Dismiss all" — see bulkTagHistoryIDs' own comment.
func (s *Store) handleBulkDismissTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	ids, ok := bulkTagHistoryIDs(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Return immediately — same fire-and-forget rationale as handleBulkAcceptTagHistory.
	w.WriteHeader(http.StatusNoContent)
	go func() {
		rows, err := s.db.Query(
			"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = ANY($1) AND owner_subject = $2 AND status = 'suggested' RETURNING message_id, source",
			ids, owner,
		)
		if err != nil {
			log.Printf("bulk-dismiss: %v", err)
			return
		}
		type pair struct{ messageID, source string }
		var pairs []pair
		for rows.Next() {
			var p pair
			if rows.Scan(&p.messageID, &p.source) == nil {
				pairs = append(pairs, p)
			}
		}
		rows.Close()
		var wg sync.WaitGroup
		var mu sync.Mutex
		anyRestored := false
		for _, p := range pairs {
			wg.Add(1)
			go func(p pair) {
				defer wg.Done()
				if s.restoreFromJunkIfSpamDeclined(owner, p.messageID, p.source) {
					mu.Lock()
					anyRestored = true
					mu.Unlock()
				}
			}(p)
		}
		wg.Wait()
		if anyRestored {
			s.broadcaster.publish("mail")
		}
	}()
}

// handleBulkClearTagHistory is "Clear list" — see bulkTagHistoryIDs' own comment. No
// side effects beyond the delete itself (see handleClearTagHistory's own comment on
// why clearing doesn't touch suppressedTags or Junk restoration).
func (s *Store) handleBulkClearTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	ids, ok := bulkTagHistoryIDs(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.db.Exec(
		"DELETE FROM tag_history WHERE id = ANY($1) AND owner_subject = $2 AND status = 'suggested'",
		ids, owner,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restoreFromJunkIfSpamDeclined undoes the one real-world side effect a spam verdict
// can have once the user has explicitly overridden it (dismissing a suggestion, or
// undoing a full-auto apply — Spam has instant_move=true, so an applied decision
// likely already moved the mail for real). Whatever actually put the mail in Junk
// might not even have been Sift (a server-side filter on the mail host is common) —
// but once the user has said "no, this isn't spam" within Sift, leaving it stranded
// in Junk contradicts that verdict regardless of how it got there. Scoped to the
// account's detected Junk folder specifically, and to spam_engine decisions only —
// undoing a *tagging* decision doesn't imply "this belongs in the inbox" the way
// declining a spam verdict does.
//
// Deliberately does NOT publish "mail" itself, even though every single-action caller
// used to rely on it doing exactly that — a real run of the bulk callers (the
// restore-stranded/cleanup-unconfirmed jobs, bulk dismiss/undo) calls this once per
// message, and a "mail" publish fans out to every connected client's live-update
// listener, which refetches its entire current view — 15 real restores meant 15 full
// refetches of whatever the client happened to be looking at, which is exactly the
// "almost 1000 requests" pattern this was mistaken for at first. Every caller now
// publishes its own single event once its whole batch (one message, or many) is done.
func (s *Store) restoreFromJunkIfSpamDeclined(owner, messageID, source string) bool {
	if source != "spam_engine" || messageID == "" {
		return false
	}

	// Fast path: the local mails cache already has a row for this message, so its
	// current folder and cache id are right there — the common case once an account's
	// been running a while.
	var mailID, accountID, folder string
	err := s.db.QueryRow(
		"SELECT id, coalesce(account_id, ''), folder FROM mails WHERE message_id = $1 LIMIT 1", messageID,
	).Scan(&mailID, &accountID, &folder)
	if err == nil && accountID != "" {
		if _, _, junk, err := s.resolveFolders(accountID); err == nil && junk != "" && folder == junk {
			if err := s.moveMailToFolder(mailID, "INBOX"); err != nil {
				log.Printf("restore from junk %s: %v", messageID, err)
				return false
			}
			s.db.Exec("DELETE FROM mails WHERE id = $1", mailID) // matches handleMove's own cleanup after a successful move
			return true
		}
		return false
	}

	// Slow path: no local cache row for this message at all — syncAccount only ever
	// populates INBOX, every other folder (Junk included) is filled in by the periodic
	// background walk, which a freshly connected or just-restored-from-backup account
	// hasn't necessarily reached yet. Relying on the cache here meant declining a
	// suggestion silently did nothing on a fresh account — no error, the dismiss itself
	// still succeeded, just no actual move, which is worse than a visible failure.
	// Search every one of the owner's accounts directly over IMAP instead.
	rows, err := s.db.Query("SELECT id, host, port, username, oauth_provider FROM accounts WHERE owner_subject = $1", owner)
	if err != nil {
		return false
	}
	type acctRow struct {
		id, host, username, oauthProvider string
		port                              int
	}
	var accts []acctRow
	for rows.Next() {
		var a acctRow
		if rows.Scan(&a.id, &a.host, &a.port, &a.username, &a.oauthProvider) == nil {
			accts = append(accts, a)
		}
	}
	rows.Close()

	for _, a := range accts {
		_, _, junk, err := s.resolveFolders(a.id)
		if err != nil || junk == "" {
			continue
		}
		_, password, err := s.loadAccountCreds(a.id)
		if err != nil {
			continue
		}
		uid, err := findUIDByMessageID(a.host, a.port, a.username, password, a.oauthProvider, junk, messageID)
		if err != nil || uid == 0 {
			continue
		}
		if err := moveMail(a.host, a.port, a.username, password, a.oauthProvider, uid, junk, "INBOX"); err != nil {
			log.Printf("restore from junk (live search) %s: %v", messageID, err)
			return false
		}
		s.markSelfMovedIntoInbox(a.id) // same suppression a cache-path move gets via moveMailToFolder
		return true
	}
	return false
}

// handleBlockSenderTagHistory is the broader, explicit-opt-in sibling of dismiss: not
// just "wrong for this email" but "stop suggesting this tag for this sender, period."
// Dismisses the suggestion too (a block implies the immediate one was also wrong).
func (s *Store) handleBlockSenderTagHistory(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	var tagID, senderEmail, messageID, source string
	if err := s.db.QueryRow(
		"UPDATE tag_history SET status = 'dismissed', resolved_at = now() WHERE id = $1 AND owner_subject = $2 AND status = 'suggested' RETURNING tag_id, sender_email, message_id, source",
		id, owner,
	).Scan(&tagID, &senderEmail, &messageID, &source); err != nil {
		http.NotFound(w, r)
		return
	}
	// Backgrounded — see handleDismissTagHistory's identical comment on why this one
	// needs its own conditional publish (no unconditional one elsewhere in this handler).
	go func() {
		if s.restoreFromJunkIfSpamDeclined(owner, messageID, source) {
			s.broadcaster.publish("mail")
		}
	}()
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
	wordWeighting := "plain"
	tagAutoApply := autoApplyScore
	spamAutoApply := spamAutoApplyScore
	var backfillCompletedAt *time.Time
	s.db.QueryRow(
		"SELECT auto_tag_mode, spam_mode, auto_move_delay_days, image_cache_retention_days, image_backfill_completed_at, word_profile_weighting, tag_auto_apply_score, spam_auto_apply_score FROM owner_settings WHERE owner_subject = $1", owner,
	).Scan(&mode, &spamMode, &delay, &imageRetention, &backfillCompletedAt, &wordWeighting, &tagAutoApply, &spamAutoApply)
	writeJSON(w, map[string]any{
		"autoTagMode": mode, "spamMode": spamMode, "autoMoveDelayDays": delay,
		"imageCacheRetentionDays": imageRetention, "imageBackfillCompletedAt": backfillCompletedAt,
		"wordProfileWeighting": wordWeighting,
		"tagAutoApplyScore": tagAutoApply, "spamAutoApplyScore": spamAutoApply,
	})
}

func (s *Store) handleSetOwnerSettings(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		AutoTagMode             string  `json:"autoTagMode"`
		SpamMode                string  `json:"spamMode"`
		AutoMoveDelayDays       int     `json:"autoMoveDelayDays"`
		ImageCacheRetentionDays int     `json:"imageCacheRetentionDays"`
		WordProfileWeighting    string  `json:"wordProfileWeighting"`
		TagAutoApplyScore       float64 `json:"tagAutoApplyScore"`
		SpamAutoApplyScore      float64 `json:"spamAutoApplyScore"`
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
	req.WordProfileWeighting = "distinctive"
	if req.AutoMoveDelayDays < 0 {
		req.AutoMoveDelayDays = 0
	}
	if req.ImageCacheRetentionDays < 1 {
		req.ImageCacheRetentionDays = 1
	}
	if req.ImageCacheRetentionDays > imageBackfillMaxDays {
		req.ImageCacheRetentionDays = imageBackfillMaxDays
	}
	if req.TagAutoApplyScore == 0 {
		req.TagAutoApplyScore = s.ownerTagAutoApplyScore(owner)
	}
	if req.TagAutoApplyScore < 0.4 || req.TagAutoApplyScore > 1 {
		req.TagAutoApplyScore = autoApplyScore
	}
	if req.SpamAutoApplyScore == 0 {
		req.SpamAutoApplyScore = s.ownerSpamAutoApplyScore(owner)
	}
	if req.SpamAutoApplyScore < 0.4 || req.SpamAutoApplyScore > 1 {
		req.SpamAutoApplyScore = spamAutoApplyScore
	}
	_, err := s.db.Exec(`
		INSERT INTO owner_settings (owner_subject, auto_tag_mode, spam_mode, auto_move_delay_days, image_cache_retention_days, word_profile_weighting, tag_auto_apply_score, spam_auto_apply_score) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_subject) DO UPDATE SET auto_tag_mode = excluded.auto_tag_mode, spam_mode = excluded.spam_mode,
			auto_move_delay_days = excluded.auto_move_delay_days, image_cache_retention_days = excluded.image_cache_retention_days,
			word_profile_weighting = excluded.word_profile_weighting,
			tag_auto_apply_score = excluded.tag_auto_apply_score, spam_auto_apply_score = excluded.spam_auto_apply_score`,
		owner, req.AutoTagMode, req.SpamMode, req.AutoMoveDelayDays, req.ImageCacheRetentionDays, req.WordProfileWeighting,
		req.TagAutoApplyScore, req.SpamAutoApplyScore,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scanFolderLimit is the IMAP page size walkAccountFolders pages through a folder
// with — not a cap on how much of the folder gets scanned (a full scan pages until
// each folder is exhausted), just how many messages it fetches per round trip.
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
func (s *Store) scanAccountForTags(ctx context.Context, accountID, scope string, folders []string, suggestThreshold float64, onProgress func(done, total int)) (scanSummary, error) {
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
		body   MailBody
	}
	var allMu sync.Mutex // onMail now runs concurrently (folderwalk.go) — append isn't safe without this
	var all []mailInFolder
	// needBodies=true (was false) — Signal A/B below now also tokenizes each mail's body
	// into tag_history.body_tokens, which is what rebuildWordProfiles later aggregates
	// into each tag's word profile. Same body-fetch cost scanAccountForSpam already pays.
	err = s.walkAccountFolders(ctx, acct, password, accountID, names, scanFolderLimit, true, nil, func(_ *imapclient.Client, m Mail, body MailBody, folder string) bool {
		allMu.Lock()
		all = append(all, mailInFolder{m, folder, body})
		allMu.Unlock()
		return true // a real "scan everything" scan, not just the newest page of each folder
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
	for i, mf := range all {
		// The folder-walk phase's done/total counts folders; this is a second,
		// later stretch of the exact same SSE connection counting mails instead, which
		// the client tells apart by remembering it already saw the done=-1 switch
		// signal above. Throttled to every 100 — granular enough to look alive on a
		// mailbox in the tens of thousands without making this loop's bottleneck (DB
		// round trips per mail) noticeably worse from the extra scan_jobs row writes.
		if onProgress != nil && i%100 == 0 {
			onProgress(i, len(all))
		}
		m, folder := mf.mail, mf.folder
		if m.MessageID == "" {
			continue
		}
		if seenMessageIDs[m.MessageID] {
			continue
		}
		seenMessageIDs[m.MessageID] = true
		bodyTokens := tokenizeBody(bodyPlainText(mf.body))
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
					s.recordTagHistory(ownerSubject, accountID, m.MessageID, tagID, m.SenderEmail, m.Subject, "folder_rule", "applied", nil, bodyTokens)
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
		if scored := s.scoreTagsForMail(ownerSubject, m.MessageID, m.SenderEmail, m.Subject, bodyTokens, suggestThreshold); len(scored) > 0 {
			for _, c := range scored {
				score := c.Score
				if mode == "full_auto" && score >= s.ownerTagAutoApplyScore(ownerSubject) {
					s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, c.TagID)
					// Resolves any pending 'suggested' row for this pair to 'applied' too —
					// covers a mode switch (review then later full_auto) promoting an old
					// suggestion, not just a fresh apply.
					s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "scan_inferred", "applied", &score, bodyTokens)
					summary.Applied++
					continue
				}
				// A pending suggestion for this exact (message, tag) already exists —
				// recording another one every time a scan re-touches mail still sitting
				// unresolved from a previous scan is what put multiple identical
				// "Suggest: X" chips on one mail (the reader shows every pending
				// suggestion, with no de-dup of its own).
				var alreadySuggested bool
				s.db.QueryRow(
					"SELECT EXISTS(SELECT 1 FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested')",
					m.MessageID, c.TagID,
				).Scan(&alreadySuggested)
				if alreadySuggested {
					continue
				}
				s.recordTagHistory(ownerSubject, accountID, m.MessageID, c.TagID, m.SenderEmail, m.Subject, "scan_inferred", "suggested", &score, bodyTokens)
				summary.Suggested++
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
	// Once, at the end of the scan, not per-mail — see rebuildWordProfiles' own comment
	// on why this is the "rarely" trigger rather than something the live path also does.
	s.rebuildWordProfiles(ownerSubject)
	return summary, nil
}

// handleScanTags streams progress over SSE the same way handleSearch does — scanning
// every folder in an account is a multi-IMAP-round-trip operation that can take a
// while, and a blocking response left search in the same position before this pattern
// was adopted there. Unlike before, this connection doesn't own the scan: it either
// starts a new scan_jobs-tracked run or attaches to one already running (the
// account+kind already has a job), then just polls that job's row (subscribeScanJob,
// scanjobs.go) until it stops — a reload reconnects here and gets the real current
// progress immediately rather than nothing.
func (s *Store) handleScanTags(w http.ResponseWriter, r *http.Request, owner string) {
	accountID := r.PathValue("id")
	scope := r.URL.Query().Get("scope") // "inbox" or "" (all folders) — ignored if folders is set
	folders := r.URL.Query()["folders"] // optional: scan exactly these folders instead of scope
	suggestThreshold := suggestScore
	if t := r.URL.Query().Get("suggestThreshold"); t != "" {
		if v, err := strconv.ParseFloat(t, 64); err == nil {
			suggestThreshold = math.Max(0.1, v)
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var sendMu sync.Mutex
	sendEvent := func(event string, data any) {
		sendMu.Lock()
		defer sendMu.Unlock()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	jobID, alreadyRunning := s.findRunningScanJob(accountID, "tags")
	if !alreadyRunning {
		var err error
		jobID, err = s.startScanJob(owner, accountID, "tags")
		if err != nil {
			sendEvent("error", map[string]string{"message": err.Error()})
			return
		}
		go s.runScanJob(jobID, "tags", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
			summary, err := s.scanAccountForTags(ctx, accountID, scope, folders, suggestThreshold, onProgress)
			if err == nil && summary.Applied > 0 {
				// full_auto mode can apply tags directly during a scan — same gap as accept:
				// without this, those wouldn't show up in the inbox until something else
				// happened to trigger a refresh.
				s.broadcaster.publish("mail")
			}
			return summary, err
		})
	}
	sendEvent("job", map[string]string{"jobId": jobID})

	err := s.subscribeScanJob(r.Context(), jobID, func(snap scanJobSnapshot) {
		switch snap.Status {
		case "running":
			sendEvent("progress", map[string]int{"done": snap.Done, "total": snap.Total})
		case "done":
			sendEvent("complete", snap.Summary)
		case "error":
			sendEvent("error", map[string]string{"message": snap.Error})
		default: // cancelled, interrupted
			sendEvent("cancelled", map[string]string{})
		}
	})
	if err != nil && r.Context().Err() == nil {
		sendEvent("error", map[string]string{"message": err.Error()})
	}
}

// handleCancelScanJob stops the running job for this account+kind, if any — a no-op,
// not an error, if nothing's running (the button can't always know whether the job
// finished a moment before the click landed).
func (s *Store) handleCancelScanJob(w http.ResponseWriter, r *http.Request, owner, kind string) {
	accountID := r.PathValue("id")
	if jobID, ok := s.findRunningScanJob(accountID, kind); ok {
		s.cancelScanJob(jobID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCancelOwnerJob is handleCancelScanJob's owner-wide counterpart — for a job
// with no single account in its lookup key (handleOwnerJobSSE, scanjobs.go).
func (s *Store) handleCancelOwnerJob(w http.ResponseWriter, r *http.Request, owner, kind string) {
	if jobID, _, ok := s.findRunningScanJobForOwner(owner, kind); ok {
		s.cancelScanJob(jobID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunningScanJob is what a panel reopening after a reload calls first — finding
// a running job (and which account it belongs to) before assuming none exists, so a
// reload mid-scan doesn't quietly forget about it.
func (s *Store) handleRunningScanJob(w http.ResponseWriter, r *http.Request, owner string) {
	kind := r.URL.Query().Get("kind")
	jobID, accountID, found := s.findRunningScanJobForOwner(owner, kind)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, map[string]string{"jobId": jobID, "accountId": accountID})
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
	// Best-effort — "" on failure just means the t.id = $3 comparison below never
	// matches, same as if this owner had no Spam tag at all (falls through to normal
	// instant_move behavior for every tag, spam included).
	spamTagID, _ := s.getOrCreateSpamTag(ownerSubject)

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
	//
	// The Spam tag's own instant_move gets one further carve-out: a score confident
	// enough to auto-apply at all (spamAutoApplyScore) isn't necessarily confident
	// enough to disappear into Junk with no human ever seeing it — only a score at or
	// above spamInstantJunkScore skips the delay; anything between the two still gets
	// tagged right away (so it's visible, searchable, filtered out of view) but waits
	// out the normal delay like any non-instant tag, giving a real window to notice and
	// undo a false positive before it's gone. coalesce(...,1) treats a manually-applied
	// Spam tag (score is NULL — a human's own deliberate decision, not the scorer's) as
	// maximally confident, so it still moves instantly like today.
	rows, err := s.db.Query(`
		SELECT DISTINCT th.message_id, th.tag_id, m.id, coalesce(m.account_id, ''), m.sender, m.subject
		FROM tag_history th
		JOIN mails m ON m.message_id = th.message_id AND m.folder = 'INBOX'
		JOIN tags t ON t.id = th.tag_id
		WHERE th.owner_subject = $1 AND th.status = 'applied'
		  AND coalesce(m.sent_at, th.created_at) < now() - (
		    (CASE
		       WHEN t.instant_move AND NOT (t.id = $3 AND coalesce(th.score, 1) < $4) THEN 0
		       ELSE $2
		     END) * interval '1 day'
		  )
		LIMIT 50`,
		ownerSubject, delayDays, spamTagID, spamInstantJunkScore,
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
// already there short of moving every mail out and back in. Job-backed
// (account-scoped, like the folder scans — findRunningScanJob/startScanJob, not
// handleOwnerJobSSE's owner-wide variant) since "all" can mean paging an entire
// folder's history, real IMAP work that can outlast one SSE connection.
func (s *Store) handleApplyTagToFolder(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	folder := r.URL.Query().Get("folder")
	tagID := r.URL.Query().Get("tagId")
	if folder == "" || tagID == "" {
		http.Error(w, "folder and tagId are required", http.StatusBadRequest)
		return
	}
	var owner string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&owner); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var sendMu sync.Mutex
	sendEvent := func(event string, data any) {
		sendMu.Lock()
		defer sendMu.Unlock()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	kind := "apply-tag-folder"
	jobID, alreadyRunning := s.findRunningScanJob(accountID, kind)
	if !alreadyRunning {
		var err error
		jobID, err = s.startScanJob(owner, accountID, kind)
		if err != nil {
			sendEvent("error", map[string]string{"message": err.Error()})
			return
		}
		go s.runScanJob(jobID, kind, func(ctx context.Context, onProgress func(done, total int)) (any, error) {
			return s.applyTagToFolder(ctx, accountID, owner, folder, tagID, onProgress)
		})
	}
	sendEvent("job", map[string]string{"jobId": jobID})

	err := s.subscribeScanJob(r.Context(), jobID, func(snap scanJobSnapshot) {
		switch snap.Status {
		case "running":
			// No real "total" here — "all" means paging until the folder's exhausted,
			// not a known count up front — done doubles as "page N" instead.
			sendEvent("progress", map[string]int{"page": snap.Done})
		case "done":
			sendEvent("complete", snap.Summary)
		case "error":
			sendEvent("error", map[string]string{"message": snap.Error})
		default:
			sendEvent("cancelled", map[string]string{})
		}
	})
	if err != nil && r.Context().Err() == nil {
		sendEvent("error", map[string]string{"message": err.Error()})
	}
}

func (s *Store) applyTagToFolder(ctx context.Context, accountID, ownerSubject, folder, tagID string, onProgress func(done, total int)) (any, error) {
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return nil, err
	}
	c, err := dialIMAP(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	mbox, err := c.Select(folder, nil).Wait()
	if err != nil {
		return nil, err
	}

	// "All" means all — page back with the same UID cursor infinite scroll uses, on
	// one held-open connection, until exhausted, not just the most recent batch.
	applied := 0
	var beforeUID uint32
	for page := 1; ; page++ {
		if ctx.Err() != nil {
			break
		}
		mails, err := fetchFolderMailPage(c, accountID, mbox, folder, scanFolderLimit, beforeUID)
		if err != nil {
			return nil, err
		}
		if len(mails) == 0 {
			break
		}
		for _, m := range mails {
			// Advance the cursor for every mail regardless of tag outcome — previously this
			// only moved when a mail was newly tagged, so a page of already-tagged mail
			// (ON CONFLICT DO NOTHING → RowsAffected==0 for all) left beforeUID stuck at
			// zero and the loop fetched the same first page forever.
			if m.UID > 0 && (beforeUID == 0 || m.UID < beforeUID) {
				beforeUID = m.UID
			}
			if m.MessageID == "" {
				continue
			}
			res, err := s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, tagID)
			if err != nil {
				continue
			}
			// ON CONFLICT DO NOTHING means err is nil even when this mail already had the
			// tag — re-running "Apply to folder" (or it covering the same mail across
			// pages/runs) would otherwise log an identical "applied" row every time.
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				continue
			}
			s.recordTagHistory(ownerSubject, accountID, m.MessageID, tagID, m.SenderEmail, m.Subject, "folder_rule", "applied", nil, nil)
			applied++
		}
		if onProgress != nil {
			onProgress(page, 0)
		}
		if len(mails) < scanFolderLimit {
			break // last (oldest) page was short of a full batch — nothing left before it
		}
	}

	if applied > 0 {
		s.broadcaster.publish("mail") // same signal a new-mail push uses — tells every connected client to refresh
	}
	return map[string]int{"applied": applied}, nil
}
