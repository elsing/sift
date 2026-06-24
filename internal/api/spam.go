package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
)

// SpamRoutes registers the manual "scan this account for spam now" trigger —
// everything else (scoring, tagging, moving new mail) runs automatically off the
// existing sync pipeline; this just covers mail that arrived before spam detection did.
func (s *Store) SpamRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/accounts/{id}/scan-spam", s.handleScanSpam)
	mux.HandleFunc("POST /api/spam/restore-stranded", func(w http.ResponseWriter, r *http.Request) {
		s.handleRestoreStrandedSpam(w, r, ownerSubject(r))
	})
}

// handleRestoreStrandedSpam is the one-off catch-up for restoreFromJunkIfSpamDeclined
// (smarttags.go) — that fix only takes effect for a decision made *after* it shipped,
// so anything dismissed/undone before then is still sitting wherever it was when the
// user said "not spam". Finds every spam_engine decision marked dismissed whose mail
// is currently still sitting in that account's detected Junk folder, and moves each
// one back to INBOX for real over IMAP.
func (s *Store) handleRestoreStrandedSpam(w http.ResponseWriter, r *http.Request, owner string) {
	rows, err := s.db.Query(`
		SELECT m.id, m.account_id
		FROM tag_history th
		JOIN mails m ON m.message_id = th.message_id
		JOIN accounts a ON a.id = m.account_id
		WHERE th.owner_subject = $1 AND th.source = 'spam_engine' AND th.status = 'dismissed'
		  AND a.junk_folder IS NOT NULL AND m.folder = a.junk_folder`,
		owner,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type candidate struct{ mailID, accountID string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.mailID, &c.accountID) == nil {
			candidates = append(candidates, c)
		}
	}
	rows.Close()

	restored := 0
	for _, c := range candidates {
		if err := s.moveMailToFolder(c.mailID, "INBOX"); err != nil {
			continue // best-effort — one mail that's moved/expunged server-side since shouldn't block the rest
		}
		s.db.Exec("DELETE FROM mails WHERE id = $1", c.mailID)
		restored++
	}
	if restored > 0 {
		s.broadcaster.publish("mail")
	}
	writeJSON(w, map[string]int{"restored": restored})
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
	end := strings.IndexAny(rest, " ;\t\n")
	if end < 0 {
		end = len(rest)
	}
	verdict := strings.ToLower(strings.TrimSpace(rest[:end]))
	if verdict == "" {
		return "none"
	}
	return verdict
}

// recordSpamFlags is the single insert path for a scored mail's stored result —
// called from every place that actually runs scoreSpam (prefetchMailImages,
// scanAccountForSpam), never from a read path. The reader's bottom-of-mail readout
// (handleMailBody, mails.go) only ever reads this back; it never scores anything
// itself, so opening an email doesn't trigger a scan.
func (s *Store) recordSpamFlags(messageID, ownerSubject string, score float64, reasons []string, body MailBody) {
	s.db.Exec(
		"INSERT INTO spam_flags (id, message_id, owner_subject, score, reasons, spf, dkim, dmarc) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		// Plain newline-joined TEXT, not a postgres TEXT[] — pgx's stdlib database/sql
		// driver returns array columns as their raw string literal by default rather
		// than decoding into a Go []string, which silently broke every read of this
		// column (see migration 0030's comment for the exact error).
		randomID(), messageID, ownerSubject, score, strings.Join(reasons, "\n"),
		authResultVerdict(body.AuthenticationResults, "spf"),
		authResultVerdict(body.AuthenticationResults, "dkim"),
		authResultVerdict(body.AuthenticationResults, "dmarc"),
	)
}

// handleScanSpam streams progress over SSE, same as handleScanTags (which this
// mirrors exactly — scope/folders params included) and handleBackfillImageCache.
func (s *Store) handleScanSpam(w http.ResponseWriter, r *http.Request) {
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
	sendEvent := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	summary, err := s.scanAccountForSpam(r.Context(), accountID, scope, folders, func(done, total int) {
		sendEvent("progress", map[string]int{"done": done, "total": total})
	})
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	sendEvent("complete", map[string]int{"applied": summary.applied, "suggested": summary.suggested, "trained": summary.trained})
}

// spamScanFolderLimit mirrors scanFolderLimit (smarttags.go) — enough of a folder's
// real history to mean something, not just the most recent page.
const spamScanFolderLimit = 200

type spamScanSummary struct {
	applied, suggested, trained int
}

// scanAccountForSpam retroactively runs the same scoring prefetchMailImages already
// applies to new mail (imageproxy.go) against EXISTING mail in this account — the
// "scan now" counterpart to that live, new-mail-only path, for whatever was already
// sitting there before spam detection existed. Mirrors scanAccountForTags's own
// scope/folders handling exactly (scope=="inbox" scans just INBOX; an explicit folders
// list takes precedence over scope entirely; otherwise every folder except Trash).
// Junk/Trash are treated specially when included: mail already sitting there has
// already been sorted (by the server or a past manual move), so rather than re-running
// the heuristic scorer against it, it's recorded directly as training data
// (status=applied) — exactly what senderRatio/domainRatio (smarttags.go) read to
// strengthen the history-reinforcement signal.
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
	sortedFolders := map[string]bool{}
	if trash != "" {
		sortedFolders[trash] = true
	}
	if junk != "" {
		sortedFolders[junk] = true
	}

	var names []string
	if len(folders) > 0 {
		names = folders // an explicit pick is the user's own call — Junk/Trash included if that's what they chose
	} else if scope == "inbox" {
		names = []string{"INBOX"}
	} else {
		names, _, err = listFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
		if err != nil {
			return summary, err
		}
		// "All folders" still includes Junk/Trash here (unlike the tag scanner, which
		// excludes Trash entirely) — that's exactly the already-sorted training data
		// described above, not noise to filter out.
	}

	// Per-mail progress, not per-folder: unlike the tag scanner, scoring here does a
	// real IMAP body fetch per mail, so a single large folder (or "Inbox only" — always
	// exactly one folder) could otherwise sit at the same percentage for the entire
	// scan, then jump straight to 100% at the very end. A cheap STATUS pass up front
	// (no body fetch) gets the real per-folder message count — capped at the same
	// limit the walk itself respects — so the count shown is the actual number of
	// mails this scan will touch, not a guess.
	counts := folderMessageCounts(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, names)
	totalUnits := 0
	for _, folder := range names {
		n := counts[folder]
		if n > spamScanFolderLimit {
			n = spamScanFolderLimit
		}
		totalUnits += n
	}
	if totalUnits == 0 {
		totalUnits = 1
	}
	doneUnits := 0

	err = s.walkAccountFolders(ctx, acct, password, accountID, names, spamScanFolderLimit, func(m Mail, folder string) {
		doneUnits++
		if doneUnits > totalUnits {
			totalUnits = doneUnits // a folder grew since the STATUS pass — keep the bar honest rather than stuck over 100%
		}
		progress(doneUnits, totalUnits)

		if m.MessageID == "" || m.SenderEmail == "" {
			return
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
		body, err := withTimeout(imapOpTimeout, func() (MailBody, error) {
			return fetchMailBody(acct, password, folder, m.UID)
		})
		if err != nil {
			return
		}
		score, reasons := s.scoreSpam(ownerSubject, spamTagID, m, body)
		// Stored for every scored mail, not just ones that clear the suggest
		// threshold — this is what the reader's bottom-of-mail readout reads back
		// (mails.go), so a low-scoring mail still has something to show instead of
		// "not yet scanned."
		s.recordSpamFlags(m.MessageID, ownerSubject, score, reasons, body)
		if isGroundTruth {
			score = math.Max(score, spamAutoApplyScore)
			summary.trained++
		}
		if alreadyDecided || score < spamSuggestScore {
			return
		}
		status := "suggested"
		// Review means review — a high score never overrides the mode and applies on
		// its own (matches scanAccountForTags's identical full_auto-gated logic). Score
		// alone only takes effect once mode is already full_auto.
		if mode == "full_auto" && score >= spamAutoApplyScore {
			status = "applied"
			s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, spamTagID)
			s.ensureSpamFolderRule(accountID, spamTagID)
			summary.applied++
		} else {
			summary.suggested++
		}
		s.recordTagHistory(ownerSubject, accountID, m.MessageID, spamTagID, m.SenderEmail, m.Subject, "spam_engine", status, &score)
	}, nil)
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
	"congratulations", "act immediately", "verify your identity",
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
func (s *Store) scoreSpam(ownerSubject, spamTagID string, m Mail, body MailBody) (float64, []string) {
	var signals []spamSignal

	// Authentication is the strongest signal available and is weighted accordingly:
	// an explicit fail is bad, but *absence* of a check matters too (a real spam/abuse
	// staple — e.g. backscatter/bounce mail routinely has no SPF or DKIM record at
	// all). On top of each individual check, failing to pass ALL THREE is its own
	// combined signal — legitimate mail today almost always passes at least one, so a
	// mail that passes none of SPF/DKIM/DMARC is suspicious even if no single check
	// outright failed.
	spf := authResultVerdict(body.AuthenticationResults, "spf")
	dkim := authResultVerdict(body.AuthenticationResults, "dkim")
	dmarc := authResultVerdict(body.AuthenticationResults, "dmarc")
	authCheck := func(verdict, label string, noneWeight float64) {
		switch verdict {
		case "pass":
		case "none":
			signals = append(signals, spamSignal{noneWeight, label + " record not present"})
		default:
			signals = append(signals, spamSignal{0.35, label + " check failed"})
		}
	}
	authCheck(spf, "SPF", 0.15)
	authCheck(dkim, "DKIM", 0.15)
	// DMARC absence weighted higher than SPF/DKIM absence — DMARC adoption among
	// legitimate senders is high enough today that a real, cared-about sender having
	// no DMARC record at all is itself a meaningfully stronger tell than SPF/DKIM
	// alone being unset (which still happens for some legitimate smaller setups).
	authCheck(dmarc, "DMARC", 0.3)
	allAuthPass := spf == "pass" && dkim == "pass" && dmarc == "pass"
	anyAuthPass := spf == "pass" || dkim == "pass" || dmarc == "pass"
	if !anyAuthPass {
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

	text := strings.ToLower(m.Subject + " " + body.Text)
	if body.Text == "" && body.HTML != "" {
		text = strings.ToLower(m.Subject + " " + html.UnescapeString(htmlTagPattern.ReplaceAllString(body.HTML, " ")))
	}
	var matchedPhrases []string
	for _, phrase := range spamPhrases {
		if strings.Contains(text, phrase) {
			matchedPhrases = append(matchedPhrases, phrase)
		}
	}
	if len(matchedPhrases) > 0 {
		signals = append(signals, spamSignal{
			weight: math.Min(0.5, float64(len(matchedPhrases))*0.15),
			reason: fmt.Sprintf("Contains spam phrase(s): %s", strings.Join(matchedPhrases, ", ")),
		})
	}

	if r := capsRatio(m.Subject); r > 0.6 && len(m.Subject) > 8 {
		signals = append(signals, spamSignal{0.15, "Subject is mostly capital letters"})
	}

	if reason := anchorMismatch(body.HTML); reason != "" {
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
	// prior tag_history at all (ratio() floors at 0 with zero samples).
	if spamTagID != "" {
		hist := math.Max(s.senderRatio(ownerSubject, m.SenderEmail, spamTagID), s.domainRatio(ownerSubject, fromDomain, spamTagID))
		if hist > 0.1 {
			signals = append(signals, spamSignal{hist * 0.3, "This sender or domain has been flagged as spam before"})
		}
	}

	score := 0.0
	reasons := make([]string, 0, len(signals))
	for _, sig := range signals {
		score += sig.weight
		reasons = append(reasons, sig.reason)
	}
	if score > 1 {
		score = 1
	}
	return score, reasons
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

// anchorMismatch looks for an <a> tag whose visible text itself looks like a URL/domain
// that's different from where the link actually goes — "click here, paypal.com" that
// actually points somewhere else entirely is a classic phishing tell. Returns "" if
// nothing matches (including simply not finding any such anchor).
func anchorMismatch(htmlBody string) string {
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
