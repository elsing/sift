package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math"
	"net"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-msgauth/dkim"
)

// SpamRoutes registers the manual "scan this account for spam now" trigger —
// everything else (scoring, tagging, moving new mail) runs automatically off the
// existing sync pipeline; this just covers mail that arrived before spam detection did.
func (s *Store) SpamRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/accounts/{id}/scan-spam", func(w http.ResponseWriter, r *http.Request) {
		s.handleScanSpam(w, r, ownerSubject(r))
	})
	// See handleScanTags' identical sibling endpoint (smarttags.go) for why this is its
	// own explicit action now rather than just closing the SSE connection.
	mux.HandleFunc("POST /api/accounts/{id}/scan-spam/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelScanJob(w, r, ownerSubject(r), "spam")
	})
	// GET, not POST — EventSource (the SSE progress stream) can only ever issue GET.
	mux.HandleFunc("GET /api/spam/restore-stranded", func(w http.ResponseWriter, r *http.Request) {
		s.handleRestoreStrandedSpam(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/spam/restore-stranded/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelOwnerJob(w, r, ownerSubject(r), "restore-stranded-spam")
	})
	mux.HandleFunc("GET /api/spam/cleanup-unconfirmed", func(w http.ResponseWriter, r *http.Request) {
		s.handleCleanupUnconfirmedSpam(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/spam/cleanup-unconfirmed/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelOwnerJob(w, r, ownerSubject(r), "cleanup-unconfirmed-spam")
	})
}

// handleRestoreStrandedSpam is the one-off catch-up for restoreFromJunkIfSpamDeclined
// (smarttags.go) — that fix only takes effect for a decision made *after* it shipped,
// so anything dismissed/undone before then is still sitting wherever it was when the
// user said "not spam". Finds every spam_engine decision marked dismissed whose mail
// is currently still sitting in that account's detected Junk folder, and moves each
// one back to INBOX for real over IMAP. Job-backed (handleOwnerJobSSE, scanjobs.go) —
// this loops over every account doing real IMAP work per message, easily slow enough
// at real scale to need persistence and progress, not a single blocking request.
func (s *Store) handleRestoreStrandedSpam(w http.ResponseWriter, r *http.Request, owner string) {
	s.handleOwnerJobSSE(w, r, owner, "restore-stranded-spam", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
		return s.restoreStrandedSpam(ctx, owner, onProgress)
	})
}

func (s *Store) restoreStrandedSpam(ctx context.Context, owner string, onProgress func(done, total int)) (any, error) {
	// Every distinct message this owner has declined a spam verdict on, regardless of
	// what the local mails cache currently has for it — restoreFromJunkIfSpamDeclined
	// (smarttags.go) falls back to a live IMAP search by Message-ID when the cache has
	// no row, which matters here specifically: a freshly restored-from-backup account
	// has tag_history but its mails cache is still catching up folder by folder, so
	// joining against the cache (the original version of this query) silently skipped
	// exactly the mail this button exists to fix.
	rows, err := s.db.Query(
		"SELECT DISTINCT message_id FROM tag_history WHERE owner_subject = $1 AND source = 'spam_engine' AND status = 'dismissed'",
		owner,
	)
	if err != nil {
		return nil, err
	}
	var messageIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			messageIDs = append(messageIDs, id)
		}
	}
	rows.Close()

	restored := 0
	for i, id := range messageIDs {
		if ctx.Err() != nil {
			break
		}
		if s.restoreFromJunkIfSpamDeclined(owner, id, "spam_engine") {
			restored++
		}
		if onProgress != nil {
			onProgress(i+1, len(messageIDs))
		}
	}
	// Once for the whole batch, not once per message — see restoreFromJunkIfSpamDeclined's
	// own comment (smarttags.go) on why it no longer publishes itself.
	if restored > 0 {
		s.broadcaster.publish("mail")
	}
	return map[string]int{"restored": restored}, nil
}

// handleCleanupUnconfirmedSpam undoes a Spam tag nothing ever actually decided —
// real example that surfaced this: three ordinary Evri parcel-tracking emails sitting
// in Junk for an unrelated reason, repeatedly re-confirmed as Spam by two automatic
// mechanisms (the spam scanner's own Junk/Trash "ground truth" boost, and the tag
// scanner's folder_rule signal reapplying Spam to anything sitting in the Junk
// folder_tag_rule's destination) — neither one a real decision by this owner, full-auto
// mode, or a confident organic score. A message qualifies only when EVERY 'applied'
// tag_history row backing its current Spam tag is one of those two automatic shapes —
// any genuine accept, manual apply, or organically-scored full-auto apply leaves it
// alone. Mirrors handleRestoreStrandedSpam: untags, restores from Junk if it's still
// sitting there, and removes the now-stale history rows so they stop being read as
// "this sender has been flagged as spam before" too. Job-backed for the same reason —
// a real run of this touched 499 messages, each with its own DB/IMAP round trip.
func (s *Store) handleCleanupUnconfirmedSpam(w http.ResponseWriter, r *http.Request, owner string) {
	s.handleOwnerJobSSE(w, r, owner, "cleanup-unconfirmed-spam", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
		return s.cleanupUnconfirmedSpam(ctx, owner, onProgress)
	})
}

func (s *Store) cleanupUnconfirmedSpam(ctx context.Context, owner string, onProgress func(done, total int)) (any, error) {
	spamTagID, err := s.getOrCreateSpamTag(owner)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT th.message_id
		FROM tag_history th
		JOIN message_tags mt ON mt.message_id = th.message_id AND mt.tag_id = th.tag_id
		WHERE th.owner_subject = $1 AND th.tag_id = $2 AND th.status = 'applied'
		GROUP BY th.message_id
		HAVING bool_and(th.source = 'folder_rule' OR (th.source = 'spam_engine' AND th.score = $3))`,
		owner, spamTagID, spamAutoApplyScore,
	)
	if err != nil {
		return nil, err
	}
	var messageIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			messageIDs = append(messageIDs, id)
		}
	}
	rows.Close()

	untagged, movedBack, deletedHistory := 0, 0, 0
	for i, id := range messageIDs {
		if ctx.Err() != nil {
			break
		}
		if res, err := s.db.Exec("DELETE FROM message_tags WHERE message_id = $1 AND tag_id = $2", id, spamTagID); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				untagged++
			}
		}
		if s.restoreFromJunkIfSpamDeclined(owner, id, "spam_engine") {
			movedBack++
		}
		if res, err := s.db.Exec(
			"DELETE FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'applied' AND (source = 'folder_rule' OR (source = 'spam_engine' AND score = $3))",
			id, spamTagID, spamAutoApplyScore,
		); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				deletedHistory += int(n)
			}
		}
		if onProgress != nil {
			onProgress(i+1, len(messageIDs))
		}
	}
	if untagged > 0 {
		s.broadcaster.publish("mail")
	}
	return map[string]int{"untagged": untagged, "movedBack": movedBack, "deletedHistory": deletedHistory}, nil
}

// authResultVerdict pulls e.g. "pass"/"fail"/"softfail"/"neutral" out of a raw
// Authentication-Results header for one check ("spf", "dkim", or "dmarc") — "none" if
// that check isn't present in the header at all (most often because the header itself
// is missing, e.g. internal mail that never crossed a server that adds one).
func authResultVerdict(raw, check string) string {
	idx := strings.Index(strings.ToLower(raw), check+"=")
	if idx < 0 {
		return "none"
	}
	rest := raw[idx+len(check)+1:]
	// "(" is a delimiter too — real-world headers commonly attach a policy comment
	// right after the verdict with no space, e.g. "dmarc=pass(p=reject) dis=none".
	// Without this, that whole parenthetical got captured as part of the verdict
	// ("pass(p=reject)"), which then matched neither "pass" nor "none" below and fell
	// through to the failure case — a real pass scored and displayed as a failure.
	end := strings.IndexAny(rest, " (;\t\n")
	if end < 0 {
		end = len(rest)
	}
	verdict := strings.ToLower(strings.TrimSpace(rest[:end]))
	if verdict == "" {
		return "none"
	}
	return verdict
}

// dkimRecheckPasses re-verifies a DKIM signature live, against the public key DNS
// publishes right now. Unlike SPF (needs the original connecting IP, which isn't
// reliably recoverable after the fact) or DMARC (inherits SPF's problem), DKIM signs
// the message content itself — the same bytes are right here, so re-checking now is
// just as valid as checking at delivery time, as long as the sender hasn't rotated
// their key since. permerror is deliberately excluded: that means the published
// DKIM policy was malformed, not that a lookup timed out, so retrying it tells us
// nothing new.
func dkimRecheckPasses(raw []byte) bool {
	// net.LookupTXT has no built-in deadline — on a slow/unreachable nameserver this
	// runs inline in the live-sync path for every temperror message, so an unbounded
	// hang here means an unbounded hang for that account's whole sync. 3s is generous
	// for a single TXT lookup; missing the window just falls back to the existing
	// temperror dampening, same as if the recheck were never attempted.
	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return net.DefaultResolver.LookupTXT(ctx, domain)
		},
	})
	if err != nil {
		return false
	}
	for _, v := range verifications {
		if v.Err == nil {
			return true
		}
	}
	return false
}

// bodyPlainText returns a mail's plain-text body, falling back to HTML with tags
// stripped when there's no text part — the same fallback scoreSpam's own text variable
// and rebuildWordProfiles' tokenizer both need.
func bodyPlainText(body MailBody) string {
	if body.Text != "" {
		return body.Text
	}
	if body.HTML != "" {
		return html.UnescapeString(htmlTagPattern.ReplaceAllString(body.HTML, " "))
	}
	return ""
}

// recordSpamFlags is the single insert path for a scored mail's stored result —
// called from every place that actually runs scoreSpam (prefetchMailImages,
// scanAccountForSpam), never from a read path. The reader's bottom-of-mail readout
// (handleMailBody, mails.go) only ever reads this back; it never scores anything
// itself, so opening an email doesn't trigger a scan.
func (s *Store) recordSpamFlags(messageID, ownerSubject, senderEmail string, score float64, reasons []string, spf, dkim, dmarc string) {
	s.db.Exec(
		"INSERT INTO spam_flags (id, message_id, owner_subject, score, reasons, spf, dkim, dmarc, sender_email, sender_domain) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)",
		// Plain newline-joined TEXT, not a postgres TEXT[] — pgx's stdlib database/sql
		// driver returns array columns as their raw string literal by default rather
		// than decoding into a Go []string, which silently broke every read of this
		// column (see migration 0030's comment for the exact error).
		randomID(), messageID, ownerSubject, score, strings.Join(reasons, "\n"),
		spf, dkim, dmarc, senderEmail, domainOf(senderEmail),
	)
}

// handleScanSpam streams progress over SSE, same as handleScanTags (which this
// mirrors exactly, including the scan_jobs persistence — see its own comment,
// smarttags.go, for why this connection no longer owns the scan itself).
func (s *Store) handleScanSpam(w http.ResponseWriter, r *http.Request, owner string) {
	accountID := r.PathValue("id")
	scope := r.URL.Query().Get("scope")
	folders := r.URL.Query()["folders"]
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

	jobID, alreadyRunning := s.findRunningScanJob(accountID, "spam")
	if !alreadyRunning {
		var err error
		jobID, err = s.startScanJob(owner, accountID, "spam")
		if err != nil {
			sendEvent("error", map[string]string{"message": err.Error()})
			return
		}
		go s.runScanJob(jobID, "spam", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
			summary, err := s.scanAccountForSpam(ctx, accountID, scope, folders, onProgress)
			if err != nil {
				return nil, err
			}
			// spamScanSummary's fields are unexported (package-internal) — map to the
			// same public shape the client's always read, rather than storing a struct
			// that'd marshal to "{}" with nothing in it.
			return map[string]int{"applied": summary.applied, "suggested": summary.suggested, "trained": summary.trained}, nil
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

// spamScanFolderLimit mirrors scanFolderLimit (smarttags.go) — the IMAP page size,
// not a cap on how much of a folder gets scanned.
const spamScanFolderLimit = 200

type spamScanSummary struct {
	applied, suggested, trained int
}

// scanAccountForSpam retroactively runs the same scoring prefetchMailImages already
// applies to new mail (imageproxy.go) against EXISTING mail in this account — the
// "scan now" counterpart to that live, new-mail-only path, for whatever was already
// sitting there before spam detection existed. Mirrors scanAccountForTags's own
// scope/folders handling (scope=="inbox" scans just INBOX; an explicit folders list
// takes precedence over scope entirely; otherwise every folder except Trash).
// Junk is treated specially when included: mail already sitting there has already
// been sorted (by the server or a past manual move), so rather than re-running the
// heuristic scorer against it, it's recorded directly as training data
// (status=applied) — exactly what senderRatio/domainRatio (smarttags.go) read to
// strengthen the history-reinforcement signal. Trash deliberately gets no such
// treatment, and is excluded from "all folders" entirely — unlike Junk, sitting in
// Trash isn't a spam verdict at all, just "deleted for any reason", and treating it as
// confirmed-spam evidence produced real false suggestions on ordinary deleted mail.
func (s *Store) scanAccountForSpam(ctx context.Context, accountID, scope string, folders []string, progress func(done, total int)) (spamScanSummary, error) {
	var summary spamScanSummary
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return summary, err
	}
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		return summary, err
	}
	spamTagID, err := s.getOrCreateSpamTag(ownerSubject)
	if err != nil {
		return summary, err
	}
	// Created proactively, not just inside the auto-apply branch below — in Review
	// mode nothing there ever runs (everything's only ever suggested, never applied),
	// which left no destination folder for autoMoveTaggedMail to use once a
	// suggestion eventually got accepted by hand.
	s.ensureSpamFolderRule(accountID, spamTagID)
	mode := s.ownerSpamMode(ownerSubject)

	// Clear this account's own previous spam suggestions before generating fresh ones —
	// same reasoning as scanAccountForTags's identical dedup step.
	s.db.Exec(
		"DELETE FROM tag_history WHERE owner_subject = $1 AND account_id = $2 AND source = 'spam_engine' AND status = 'suggested'",
		ownerSubject, accountID,
	)

	_, trash, junk, _ := s.resolveFolders(accountID)
	// Junk only, not Trash — Junk is a real, specific verdict (this is where spam
	// goes), but Trash just means "deleted for any reason", which produced real bad
	// suggestions: a real mail trashed for being old, or a newsletter trashed after
	// reading, isn't evidence of spam, but used to get auto-scored as if it were.
	sortedFolders := map[string]bool{}
	if junk != "" {
		sortedFolders[junk] = true
	}
	const reviewFolder = "Review"
	// Best-effort: ignore error — most likely the folder already exists.
	createFolder(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, reviewFolder)

	var names []string
	if len(folders) > 0 {
		names = folders // an explicit pick is the user's own call
	} else if scope == "inbox" {
		names = []string{"INBOX"}
	} else {
		names, _, err = listFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
		if err != nil {
			return summary, err
		}
		// "All folders" still includes Junk here (the already-sorted training data
		// described above) but now excludes Trash entirely, matching the tag scanner's
		// own exclusion — Trash isn't a meaningful spam signal, just noise.
		if trash != "" {
			filtered := names[:0]
			for _, n := range names {
				if n != trash {
					filtered = append(filtered, n)
				}
			}
			names = filtered
		}
	}

	// Per-mail progress, not per-folder: unlike the tag scanner, scoring here does a
	// real IMAP body fetch per mail, so a single large folder (or "Inbox only" — always
	// exactly one folder) could otherwise sit at the same percentage for the entire
	// scan, then jump straight to 100% at the very end. A cheap STATUS pass up front
	// (no body fetch) gets the real per-folder message count, so the count shown is
	// the actual number of mails this scan will touch, not a guess.
	counts := folderMessageCounts(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, names)
	totalUnits := 0
	for _, folder := range names {
		totalUnits += counts[folder]
	}
	if totalUnits == 0 {
		totalUnits = 1
	}
	doneUnits := 0
	// onMail below now runs on several messages at once (messageProcessingConcurrency,
	// folderwalk.go) — every piece of shared state it touches (the progress counters,
	// the SSE write progress triggers, summary's fields) needs this same mutex rather
	// than the single-threaded assumption the sequential version got away with.
	var mu sync.Mutex

	err = s.walkAccountFolders(ctx, acct, password, accountID, names, spamScanFolderLimit, true, nil, func(c *imapclient.Client, m Mail, body MailBody, folder string) bool {
		mu.Lock()
		doneUnits++
		if doneUnits > totalUnits {
			totalUnits = doneUnits // a folder grew since the STATUS pass — keep the bar honest rather than stuck over 100%
		}
		d, t := doneUnits, totalUnits
		mu.Unlock()
		progress(d, t)

		if m.MessageID == "" || m.SenderEmail == "" {
			return true
		}
		// Already tagged, or already dismissed for this exact mail — the *decision*
		// never gets relitigated just because a scan ran again (a scan never removes a
		// tag it thinks is wrong, and a dismissed verdict on this mail should stay
		// dismissed). The score itself still gets refreshed below either way, so a
		// scan always leaves every mail it touches with current diagnostic data —
		// running it is the one thing that's supposed to fix "not yet scanned".
		var alreadyDecided bool
		s.db.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM message_tags WHERE message_id = $1 AND tag_id = $2) OR EXISTS(SELECT 1 FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'dismissed')",
			m.MessageID, spamTagID,
		).Scan(&alreadyDecided)

		// Every mail gets actually scored — including Junk/Trash. Mail already sitting
		// there isn't exempted from running through scoreSpam (it still needs a real
		// score/SPF/DKIM/DMARC readout, same as anything else), it just additionally
		// counts as its own evidence regardless of what the heuristic score comes out
		// to. That's still not the same thing as the user's own confirmation, though —
		// it goes through the exact same suggest/apply-by-mode gate below as any other
		// mail, rather than silently applying the tag on its own.
		isGroundTruth := sortedFolders[folder]
		// body is pre-fetched for the whole page in one batched IMAP command (see
		// walkAccountFolders) — parseMailBody always sets Text or HTML to something
		// non-empty for a message it actually parsed, so both blank is the signal this
		// particular UID's fetch/parse failed (best-effort, same as the old per-message
		// fetch-error path) rather than a message that's genuinely empty.
		if body.Text == "" && body.HTML == "" {
			return true
		}
		// Same free backfill as prefetchMailImages' live path (imageproxy.go) — this
		// already fetches the full body for every mail it scores, so a full "Scan for
		// spam" run also catches up the snippet for historical mail that arrived
		// before the live path existed, or that lives in a folder it never covers.
		s.db.Exec("UPDATE mails SET snippet = $1 WHERE id = $2", snippetFromBody(body, 200), m.ID)
		bodyTokens := tokenizeBody(bodyPlainText(body)) // feeds rebuildWordProfiles below — the Spam tag's own word profile
		score, reasons, spf, dkim, dmarc := s.scoreSpam(ownerSubject, spamTagID, m, body)
		// Stored for every scored mail, not just ones that clear the suggest
		// threshold — this is what the reader's bottom-of-mail readout reads back
		// (mails.go), so a low-scoring mail still has something to show instead of
		// "not yet scanned."
		s.recordSpamFlags(m.MessageID, ownerSubject, m.SenderEmail, score, reasons, spf, dkim, dmarc)
		if isGroundTruth && score < spamSuggestScore {
			// In Junk but genuinely clean — move to Review instead of suggesting spam.
			// Use moveMail directly: m.ID may not be in the DB yet (concurrent-lane
			// upsert races can delete a Junk row before this callback fires).
			if err := moveMail(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, m.UID, folder, reviewFolder); err != nil {
				log.Printf("move clean Junk mail %s to Review: %v", m.ID, err)
			}
			return true
		}
		if isGroundTruth {
			score = math.Max(score, spamAutoApplyScore)
			mu.Lock()
			summary.trained++
			mu.Unlock()
		}
		if alreadyDecided || score < spamSuggestScore {
			if !alreadyDecided {
				// A still-pending suggestion isn't a decision (alreadyDecided already
				// excludes those) — it's the engine's own guess from a previous scan, and
				// if a fresh score no longer clears the bar (sender history catching up,
				// a temperror that got live-rechecked since, anything else that changed),
				// the guess no longer holds either. Leaving it sitting in the queue is
				// what put a 4%-scoring, fully-passing mail in front of the user asking
				// them to confirm "spam" for something the engine itself no longer thinks
				// is spam.
				s.db.Exec(
					"DELETE FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested'",
					m.MessageID, spamTagID,
				)
			}
			return true
		}
		// Review means review — a high score never overrides the mode and applies on
		// its own (matches scanAccountForTags's identical full_auto-gated logic). Score
		// alone only takes effect once mode is already full_auto.
		if mode == "full_auto" && score >= spamAutoApplyScore {
			s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, spamTagID)
			s.ensureSpamFolderRule(accountID, spamTagID)
			mu.Lock()
			summary.applied++
			mu.Unlock()
			// recordTagHistory's own "applied" branch resolves any pending 'suggested' row
			// for this exact pair to 'applied' too — covers a mode switch (review then
			// later full_auto) promoting an old suggestion, not just a fresh apply.
			s.recordTagHistory(ownerSubject, accountID, m.MessageID, spamTagID, m.SenderEmail, m.Subject, "spam_engine", "applied", &score, bodyTokens)
			return true
		}
		// A pending suggestion for this exact (message, tag) already exists — recording
		// another one every time a scan re-touches mail that's still sitting unresolved
		// from a previous scan is exactly what put 4 identical "Suggest: Spam" chips on
		// one mail (the reader shows every pending suggestion, with no de-dup of its own).
		var alreadySuggested bool
		s.db.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested')",
			m.MessageID, spamTagID,
		).Scan(&alreadySuggested)
		if alreadySuggested {
			return true
		}
		mu.Lock()
		summary.suggested++
		mu.Unlock()
		s.recordTagHistory(ownerSubject, accountID, m.MessageID, spamTagID, m.SenderEmail, m.Subject, "spam_engine", "suggested", &score, bodyTokens)
		return true
	}, nil)
	if err == nil {
		s.rebuildWordProfiles(ownerSubject) // once, at the end of the scan — see its own comment on the "rarely" design
	}
	if err != nil {
		return summary, err
	}
	return summary, nil
}

// Separate thresholds from the regular tagging engine's (autoApplyScore/suggestScore,
// smarttags.go) — a spam false positive burying real mail is a worse failure mode than
// a missed/mis-suggested tag, so this is tuned more conservatively rather than sharing
// constants that happen to have the same names.
const (
	spamAutoApplyScore = 0.75
	spamSuggestScore   = 0.4
	// spamInstantJunkScore is the extra bar (above spamAutoApplyScore) a full-auto
	// apply needs to clear before autoMoveTaggedMail (smarttags.go) skips the normal
	// delay for it — between the two, the mail still gets tagged Spam right away, just
	// not whisked into Junk unseen, giving a real window to catch a false positive
	// before it's gone.
	spamInstantJunkScore = 0.85
)

// A small, static list of "common techniques" — these work on a brand-new sender with
// zero history, which is the whole gap a pure tagging-history engine has. Deliberately
// not exhaustive; this is a heuristic net, not a full spam-filter reimplementation.
var spamPhrases = []string{
	"verify your account", "act now", "wire transfer", "claim your prize",
	"urgent action required", "account has been suspended", "click here to confirm",
	"limited time offer", "you have won", "free gift", "no cost to you",
	"this is not spam", "dear customer", "dear valued customer",
	"bank account details", "social security number", "tax refund", "you have been selected",
	"act immediately", "verify your identity",
}

// Brand names commonly spoofed in a display name while the actual sending domain has
// nothing to do with the real company — classic phishing.
var spamBrandWords = []string{
	"paypal", "amazon", "apple", "microsoft", "netflix", "irs", "dhl", "ups", "fedex",
	"bank of america", "wells fargo", "chase", "docusign",
}

// A handful of well-known shortener domains — not exhaustive, just the ones common
// enough that seeing one at all is itself a (weak) signal.
var shortenerLink = regexp.MustCompile(`(?i)https?://(bit\.ly|tinyurl\.com|t\.co|goo\.gl|ow\.ly|is\.gd|buff\.ly|rebrand\.ly|cutt\.ly)/`)

// Three+ repeated '!' or '$' — SpamAssassin-style punctuation-run detection.
var punctSpamRun = regexp.MustCompile(`[!$]{3,}`)

// A real brand/finance word immediately next to "secure", "verify", "login", "account",
// or a run of digits, joined by hyphens — the shape of a throwaway phishing domain
// rather than an organization's real one.
var suspiciousDomainPattern = regexp.MustCompile(`(?i)(secure|verify|login|account|update|confirm)-[a-z0-9-]+\.[a-z]{2,}|[a-z]+-(paypal|amazon|apple|microsoft|bank|netflix)[a-z0-9-]*\.[a-z]{2,}`)

var (
	ipLiteralLink = regexp.MustCompile(`https?://\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	anchorTagRe   = regexp.MustCompile(`(?is)<a[^>]+href=["']([^"']+)["'][^>]*>(.*?)</a>`)
)

type spamSignal struct {
	weight float64
	reason string
}

// scoreSpam combines header-authentication failures, sender/reply-to mismatches,
// display-name spoofing, and content heuristics — the "common techniques" that work
// even against a sender with zero history — with a much smaller reinforcing signal
// from this owner's own tag_history against the Spam tag (a sender already confirmed
// spam before scores a little higher next time, but contributes nothing for a brand-
// new sender, which is exactly the gap a pure history-based engine has on its own).
// senderAuthPassRate reads this exact sender's own history of passing one specific
// auth check (column is "spf"/"dkim"/"dmarc") from spam_flags — denormalized there the
// same way tag_history denormalizes sender_email/domain, so this survives the source
// mail being archived/deleted. samples is the total scored mail on record for this
// sender, regardless of verdict; callers should require a real sample size before
// trusting rate (a sender seen once isn't a track record).
func (s *Store) senderAuthPassRate(ownerSubject, senderEmail, column string) (rate float64, samples int) {
	if senderEmail == "" {
		return 0, 0
	}
	var pass int
	s.db.QueryRow(
		"SELECT count(*) FILTER (WHERE "+column+" = 'pass'), count(*) FROM spam_flags WHERE owner_subject = $1 AND sender_email = $2",
		ownerSubject, senderEmail,
	).Scan(&pass, &samples)
	if samples == 0 {
		return 0, 0
	}
	return float64(pass) / float64(samples), samples
}

func (s *Store) scoreSpam(ownerSubject, spamTagID string, m Mail, body MailBody) (score float64, reasons []string, spfVerdict, dkimDisplay, dmarcVerdict string) {
	var signals []spamSignal

	// Authentication is the strongest signal available and is weighted accordingly:
	// an explicit fail is bad, but *absence* of a check matters too (a real spam/abuse
	// staple — e.g. backscatter/bounce mail routinely has no SPF or DKIM record at
	// all). On top of each individual check, failing to pass ALL THREE is its own
	// combined signal — legitimate mail today almost always passes at least one, so a
	// mail that passes none of SPF/DKIM/DMARC is suspicious even if no single check
	// outright failed.
	spf := authResultVerdict(body.AuthenticationResults, "spf")
	dkimVerdict := authResultVerdict(body.AuthenticationResults, "dkim")
	dmarc := authResultVerdict(body.AuthenticationResults, "dmarc")
	// A live recheck only changes anything for "temperror" specifically — see
	// dkimRecheckPasses. A successful recheck is treated as a real, full-weight pass
	// (the math is just as valid now as it was then), not a discounted one.
	dkimRecheckedPass := false
	if dkimVerdict == "temperror" && len(body.RawMessage) > 0 && dkimRecheckPasses(body.RawMessage) {
		dkimVerdict = "pass"
		dkimRecheckedPass = true
	}
	inconclusive := func(v string) bool { return v == "temperror" || v == "permerror" }
	authCheck := func(verdict, label, column string, noneWeight float64) {
		var weight float64
		var reason string
		switch verdict {
		case "pass":
			return
		case "none":
			weight, reason = noneWeight, label+" record not present"
		case "temperror", "permerror":
			// The verifier failed to even complete the check (a DNS lookup timeout, most
			// often) — that's a problem on the receiving server's side, not evidence
			// about the mail itself, so it shouldn't be weighted anywhere near a genuine
			// "fail". Same tier as "none": the check just never actually ran.
			weight, reason = noneWeight, label+" check inconclusive ("+verdict+")"
		default:
			weight, reason = 0.35, label+" check failed"
		}
		// A sender with a strong track record of passing this exact check is more
		// likely having a one-off blip (a secondary sending IP, a transient DNS hiccup
		// on the receiving end) than a real change in legitimacy — dampened, not
		// ignored, since it's still genuinely possible (a compromised account, an
		// actual config regression). Needs a real sample size before trusting it; a
		// sender seen once or twice isn't a track record yet.
		if rate, samples := s.senderAuthPassRate(ownerSubject, m.SenderEmail, column); samples >= 5 && rate >= 0.9 {
			weight *= 0.4
		}
		signals = append(signals, spamSignal{weight, reason})
	}
	// SPF/DMARC can't be live-rechecked the way DKIM can (SPF needs the original
	// connecting IP, which isn't reliably recoverable after delivery; DMARC inherits
	// that same gap) — but a DKIM signature that *did* just verify live is still real
	// positive evidence about this message, so an unresolved SPF/DMARC alongside it
	// shouldn't cost anything further on top.
	if dkimRecheckedPass && inconclusive(spf) {
		signals = append(signals, spamSignal{0, "SPF check inconclusive (" + spf + ") — not held against this message since DKIM verified live"})
	} else {
		authCheck(spf, "SPF", "spf", 0.15)
	}
	if dkimRecheckedPass {
		signals = append(signals, spamSignal{0, "DKIM check inconclusive (temperror) — re-verified live, signature still valid"})
	} else {
		authCheck(dkimVerdict, "DKIM", "dkim", 0.15)
	}
	// DMARC absence weighted higher than SPF/DKIM absence — DMARC adoption among
	// legitimate senders is high enough today that a real, cared-about sender having
	// no DMARC record at all is itself a meaningfully stronger tell than SPF/DKIM
	// alone being unset (which still happens for some legitimate smaller setups).
	if dkimRecheckedPass && inconclusive(dmarc) {
		signals = append(signals, spamSignal{0, "DMARC check inconclusive (" + dmarc + ") — not held against this message since DKIM verified live"})
	} else {
		authCheck(dmarc, "DMARC", "dmarc", 0.3)
	}
	allAuthPass := spf == "pass" && dkimVerdict == "pass" && dmarc == "pass"
	anyAuthPass := spf == "pass" || dkimVerdict == "pass" || dmarc == "pass"
	// "None of these passed" only means something when at least one check actually ran
	// to a real verdict — a genuine fail, or a settled "no record published at all"
	// (none). If every single one came back temperror/permerror, nothing was checked
	// at all (a verifier-side outage for this one message, not a fact about the
	// sender) — firing this signal anyway claimed "failed authentication" for a
	// message where authentication was simply never attempted, which is what made an
	// ordinary InvestEngine email (a real sender with a long, otherwise-clean history)
	// score 74% purely from a receiving-server hiccup with no actual evidence behind it.
	allInconclusive := inconclusive(spf) && inconclusive(dkimVerdict) && inconclusive(dmarc)
	if !anyAuthPass && !allInconclusive {
		signals = append(signals, spamSignal{0.3, "No authentication method (SPF, DKIM, or DMARC) passed"})
	}

	fromAddr, fromName := parseAddressHeader(body.From)
	fromDomain := domainOf(fromAddr)
	if fromDomain == "" {
		fromDomain = domainOf(m.SenderEmail)
	}
	// A clean Reply-To/Return-Path domain mismatch alone is common for entirely
	// legitimate bulk/transactional mail sent through a third-party ESP (SES,
	// SendGrid, etc. all rewrite Return-Path for their own bounce handling) — only
	// worth full weight when authentication ISN'T otherwise clean; a properly
	// authenticated sender using a relay is the expected case, not a red flag.
	if replyAddr, _ := parseAddressHeader(body.ReplyTo); replyAddr != "" {
		if rd := domainOf(replyAddr); rd != "" && fromDomain != "" && rd != fromDomain {
			weight := 0.3
			if allAuthPass {
				weight = 0.1
			}
			signals = append(signals, spamSignal{weight, "Reply-To domain doesn't match the sender"})
		}
	}
	if returnAddr, _ := parseAddressHeader(body.ReturnPath); returnAddr != "" {
		if rd := domainOf(returnAddr); rd != "" && fromDomain != "" && rd != fromDomain {
			weight := 0.2
			if allAuthPass {
				weight = 0.05
			}
			signals = append(signals, spamSignal{weight, "Return-Path domain doesn't match the sender"})
		}
	}

	lowerName := strings.ToLower(fromName)
	for _, brand := range spamBrandWords {
		if strings.Contains(lowerName, brand) && !strings.Contains(fromDomain, strings.ReplaceAll(brand, " ", "")) {
			signals = append(signals, spamSignal{0.4, fmt.Sprintf("Claims to be %q but the sender's domain doesn't match", brand)})
			break
		}
	}

	text := strings.ToLower(m.Subject + " " + bodyPlainText(body))
	var matchedPhrases []string
	for _, phrase := range spamPhrases {
		if strings.Contains(text, phrase) {
			matchedPhrases = append(matchedPhrases, phrase)
		}
	}
	if len(matchedPhrases) > 0 {
		// Capped well below any single auth-check failure (the smallest of which is
		// 0.15 for a bare SPF/DKIM "none", and 0.35 for an actual fail) — wording alone,
		// even several matched phrases at once, is a much weaker tell than the mail's
		// actual authentication result.
		signals = append(signals, spamSignal{
			weight: math.Min(0.25, float64(len(matchedPhrases))*0.08),
			reason: fmt.Sprintf("Contains spam phrase(s): %s", strings.Join(matchedPhrases, ", ")),
		})
	}

	if r := capsRatio(m.Subject); r > 0.6 && len(m.Subject) > 8 {
		signals = append(signals, spamSignal{0.15, "Subject is mostly capital letters"})
	}

	if reason := anchorMismatch(body.HTML, fromDomain); reason != "" {
		signals = append(signals, spamSignal{0.3, reason})
	}
	if ipLiteralLink.MatchString(body.HTML) {
		signals = append(signals, spamSignal{0.25, "Contains a link to a bare IP address"})
	}
	if shortenerLink.MatchString(body.HTML) {
		// A SpamAssassin staple (URI_SHORTENER family of rules) — link shorteners hide
		// the real destination, used legitimately but disproportionately by phishing
		// to mask where a link actually goes.
		signals = append(signals, spamSignal{0.2, "Links through a URL shortener"})
	}
	if punctSpamRun.MatchString(m.Subject) || punctSpamRun.MatchString(text) {
		// Mirrors SpamAssassin's PT_RATWARE/EXCLAIM-style punctuation rules — "!!!" or
		// "$$$" runs are a cheap, well-established low-quality-mail tell.
		signals = append(signals, spamSignal{0.15, "Excessive punctuation (e.g. \"!!!\" or \"$$$\")"})
	}
	if body.Text == "" && body.HTML != "" {
		// SpamAssassin's MIME_HTML_ONLY — legitimate transactional/newsletter mail
		// usually includes a plain-text alternative; HTML-only is disproportionately
		// common in lower-quality bulk mail. Weak alone (plenty of legit mail skips
		// the text part too), so a small weight rather than a real accusation.
		signals = append(signals, spamSignal{0.05, "No plain-text alternative (HTML-only)"})
	}
	if suspiciousDomainPattern.MatchString(fromDomain) {
		// Classic phishing domain shape: a real brand name plus extra words/numbers
		// strung together with hyphens (e.g. "secure-paypal-verify2.com").
		signals = append(signals, spamSignal{0.2, "Sender domain looks auto-generated or impersonates a brand"})
	}

	// History reinforcement — secondary, contributes nothing for a sender with no
	// prior tag_history at all (ratio() floors at 0 with zero samples). excludeFolderRule
	// (true here) leaves out the Spam tag's own folder rule (Junk → Spam) — see
	// senderRatio/domainRatio's own comment (smarttags.go) for the real false positive
	// this caused: a single newsletter copy sitting in Junk for unrelated reasons
	// reinforced every future mail from that sender as "previously flagged spam", with
	// no actual spam judgment behind it.
	if spamTagID != "" {
		hist := s.senderRatio(ownerSubject, m.SenderEmail, spamTagID, true) // already 0 for a relay-generated address (looksLikeRelayAddress, smarttags.go)
		if !looksLikeRelayAddress(m.SenderEmail) {
			// Same reasoning as scoreTagsForMail's identical guard (smarttags.go): a relay
			// alias's domain isn't necessarily covered by domainRatio's own fixed
			// freemail list, so this is checked here too rather than relying on that
			// list alone to catch every relay service.
			hist = math.Max(hist, s.domainRatio(ownerSubject, fromDomain, spamTagID, true))
		}
		if hist > 0.1 {
			signals = append(signals, spamSignal{hist * 0.3, "This sender or domain has been flagged as spam before"})
		}
		// Same idea as the sender/domain reinforcement above, but against the Spam tag's
		// own word profile (rebuildWordProfiles, smarttags.go) — this mail's wording
		// resembling previously-flagged spam, regardless of sender.
		if bp := s.bodyProfileRatio(tokenizeBody(text), spamTagID); bp > 0.1 {
			signals = append(signals, spamSignal{bp * 0.3, "This mail's wording matches previously flagged spam"})
		}
	}

	score = 0.0
	reasons = make([]string, 0, len(signals))
	for _, sig := range signals {
		score += sig.weight
		reasons = append(reasons, sig.reason)
	}
	// Mail that's already carrying some other tag looks like mail someone (the user,
	// or smart-tagging) already decided was worth keeping and sorting — not proof it
	// isn't spam (a compromised sender's mail can still pick up a real tag before
	// anyone notices), so this dampens rather than excludes. Checked after summing
	// the signals above, not as one of them — it's a discount on the whole picture,
	// not a fact about this mail's wording/authentication on its own.
	if len(s.existingTagIDs(m.MessageID)) > 0 {
		score *= 0.7
	}
	if score > 1 {
		score = 1
	}
	dkimDisplay = dkimVerdict
	if dkimRecheckedPass {
		// Distinct from a plain "pass" so the UI can show *why* this one didn't cost
		// anything — the original header still says temperror, this is the live
		// recheck's verdict, not a claim that the original check actually succeeded.
		dkimDisplay = "pass (rechecked)"
	}
	return score, reasons, spf, dkimDisplay, dmarc
}

// parseAddressHeader pulls the address and display name out of a raw header value
// like `"Display Name" <addr@domain.com>` — returns ("", "") for an empty/unparsable
// value rather than erroring, since a missing header is the common case, not a bug.
func parseAddressHeader(raw string) (addr, name string) {
	if raw == "" {
		return "", ""
	}
	a, err := mail.ParseAddress(raw)
	if err != nil {
		return "", ""
	}
	return a.Address, a.Name
}

func domainOf(email string) string {
	if at := strings.LastIndex(email, "@"); at >= 0 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}

func capsRatio(s string) float64 {
	letters, caps := 0, 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
			if r >= 'A' && r <= 'Z' {
				caps++
			}
		}
	}
	if letters == 0 {
		return 0
	}
	return float64(caps) / float64(letters)
}

// espRedirectDomains are mailing-list/newsletter platforms whose whole job is
// rewriting every link in a campaign through their own click-tracking redirect domain
// before the real destination — completely standard for entirely legitimate bulk mail
// (a club newsletter's "Sponsor" link going through Mailchimp is the normal case, not
// an exception), not a phishing tell. Without this, anchorMismatch flagged nearly any
// real newsletter — the visible link text naming the real destination while the actual
// href points at the ESP's tracking domain is exactly what these platforms always do.
var espRedirectDomains = []string{
	"list-manage.com", "list-manage1.com", "list-manage2.com", "mailchimp.com",
	"campaign-archive.com", "constantcontact.com", "sendgrid.net", "hubspotlinks.com",
	"click.convertkit-mail.com", "mailgun.org", "sparkpostmail.com", "klaviyomail.com",
	"mcsv.net", "rs6.net", "cmail19.com", "cmail20.com",
}

func isESPRedirectDomain(domain string) bool {
	for _, d := range espRedirectDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// isSenderOwnDomain reports whether hrefDomain is the sender's own domain, or a
// subdomain of it (e.g. "t.mail.eurail.com" under sender domain "eurail.com") — a
// company routing its own mail through its own tracking subdomain, same idea as
// isESPRedirectDomain but for in-house tracking infrastructure rather than a named
// third-party platform. A real example that surfaced this: a Eurail email citing an
// NCSC advisory link, wrapped (like every link in the mail) through Eurail's own
// "t.mail.eurail.com" — flagged as a mismatch because the visible text named NCSC, not
// Eurail, even though the actual destination was entirely Eurail's own infrastructure.
// Generalizes better than a fixed list ever could: any company running its own
// tracking subdomain is covered, not just ones already known and named.
func isSenderOwnDomain(hrefDomain, fromDomain string) bool {
	return fromDomain != "" && (hrefDomain == fromDomain || strings.HasSuffix(hrefDomain, "."+fromDomain))
}

// anchorMismatch looks for an <a> tag whose visible text itself looks like a URL/domain
// that's different from where the link actually goes — "click here, paypal.com" that
// actually points somewhere else entirely is a classic phishing tell. Returns "" if
// nothing matches (including simply not finding any such anchor).
func anchorMismatch(htmlBody, fromDomain string) string {
	if htmlBody == "" {
		return ""
	}
	for _, match := range anchorTagRe.FindAllStringSubmatch(htmlBody, -1) {
		href := strings.TrimSpace(match[1])
		// mailto:/tel:/etc — comparing a contact-organization name in the link text
		// against an email address or phone number isn't a meaningful mismatch check
		// at all (a "Parkrun" link to mailto:info@parkrun.org is exactly what a real
		// "contact us" link looks like, not a spoofed destination).
		if !strings.HasPrefix(strings.ToLower(href), "http://") && !strings.HasPrefix(strings.ToLower(href), "https://") {
			continue
		}
		text := strings.TrimSpace(html.UnescapeString(htmlTagPattern.ReplaceAllString(match[2], "")))
		textDomain := domainLikeToken(text)
		if textDomain == "" {
			continue
		}
		hrefDomain := strings.ToLower(hostOf(href))
		if i := strings.IndexByte(hrefDomain, ':'); i >= 0 {
			hrefDomain = hrefDomain[:i] // strip a port, if any
		}
		if isESPRedirectDomain(hrefDomain) || isSenderOwnDomain(hrefDomain, fromDomain) {
			continue
		}
		if hrefDomain != "" && textDomain != hrefDomain && !strings.Contains(hrefDomain, textDomain) {
			return fmt.Sprintf("Link text says %q but actually goes to %q", textDomain, hrefDomain)
		}
	}
	return ""
}

var domainLikePattern = regexp.MustCompile(`(?i)\b([a-z0-9-]+\.(?:com|net|org|co|io|gov|edu)(?:\.[a-z]{2})?)\b`)

// domainLikeToken finds a bare domain-looking substring within link anchor text (e.g.
// "Click here: paypal.com/login") — returns "" if the text doesn't look like it's
// trying to claim a specific destination at all, which is most ordinary link text.
func domainLikeToken(text string) string {
	m := domainLikePattern.FindString(text)
	return strings.ToLower(m)
}

func hostOf(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	rawURL = strings.TrimPrefix(rawURL, "http://")
	rawURL = strings.TrimPrefix(rawURL, "https://")
	if i := strings.IndexAny(rawURL, "/?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	return rawURL
}
