package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/net/html"
)

// ImageCacheRoutes registers the manual "backfill the last N days now" trigger —
// everything else (live proxying, prefetching new mail, cleanup) needs no client-facing
// endpoint of its own.
func (s *Store) ImageCacheRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/image-cache/backfill", func(w http.ResponseWriter, r *http.Request) {
		s.handleBackfillImageCache(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/image-cache/backfill/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelOwnerJob(w, r, ownerSubject(r), "image-backfill")
	})
}

// handleBackfillImageCache is job-backed (handleOwnerJobSSE, scanjobs.go) — walking
// every already-cached mail within the retention window across every account and
// fetching its images is real, multi-IMAP-round-trip work that can take a while,
// easily long enough to outlast one SSE connection.
func (s *Store) handleBackfillImageCache(w http.ResponseWriter, r *http.Request, owner string) {
	s.handleOwnerJobSSE(w, r, owner, "image-backfill", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
		cached, err := s.backfillImageCache(ctx, owner, onProgress)
		if err != nil {
			return nil, err
		}
		return map[string]int{"imagesCached": cached}, nil
	})
}

// imageBackfillMaxDays hard-caps how far back the backfill ever looks, regardless of
// the owner's configured image_cache_retention_days — walking every IMAP folder and
// fetching every mail body in them is real cost against a remote mail server, unlike
// the cheap local-DB cleanup that setting otherwise controls, so this is capped
// separately rather than trusting an arbitrarily large configured value.
const imageBackfillMaxDays = 90

// backfillImageCache walks every folder of every one of the owner's accounts directly
// over IMAP (sharing walkAccountFolders with the Smart Tagging scanner — both need "go
// through every folder, see what's there"), fetching and caching images for anything
// within the lookback window regardless of whether the mail's ever opened. This is
// meant as a one-time bootstrap, not a routine action — ordinary new mail already gets
// its images prefetched automatically via syncAccount; records
// image_backfill_completed_at when done so the UI can say so instead of inviting a
// repeat run "just in case."
func (s *Store) backfillImageCache(ctx context.Context, ownerSubject string, progress func(done, total int)) (int, error) {
	days := imageBackfillMaxDays
	s.db.QueryRow("SELECT image_cache_retention_days FROM owner_settings WHERE owner_subject = $1", ownerSubject).Scan(&days)
	if days > imageBackfillMaxDays {
		days = imageBackfillMaxDays
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	rows, err := s.db.Query("SELECT id FROM accounts WHERE owner_subject = $1", ownerSubject)
	if err != nil {
		return 0, err
	}
	var accountIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			accountIDs = append(accountIDs, id)
		}
	}
	rows.Close()

	type accountWork struct {
		acct     dbAccount
		password string
		folders  []string
	}
	var work []accountWork
	totalFolders := 0
	for _, accountID := range accountIDs {
		acct, password, err := s.loadAccountCreds(accountID)
		if err != nil {
			continue
		}
		names, _, err := listFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
		if err != nil {
			continue
		}
		// Trash is mostly junk/spam by nature — not worth the IMAP round trips to
		// fetch and cache images for mail about to be permanently deleted anyway.
		_, trashFolder, _, _ := detectSpecialUseFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
		if trashFolder != "" {
			filtered := names[:0]
			for _, n := range names {
				if n != trashFolder {
					filtered = append(filtered, n)
				}
			}
			names = filtered
		}
		work = append(work, accountWork{acct, password, names})
		totalFolders += len(names)
	}

	imagesCached := 0
	var cacheMu sync.Mutex // onMail now runs concurrently (folderwalk.go) — the counter isn't safe without this
	foldersDone := 0
	for _, w := range work {
		// Folders are date-ordered newest-first, so the first message outside the
		// retention window means nothing older in this folder is relevant either —
		// this filter rejecting one stops the whole folder (see walkAccountFolders'
		// own comment), not just that message. A busy folder with more than one
		// page's worth of mail *inside* the window still needs every page up to that
		// point, which capping at one page used to cut short.
		withinRetention := func(m Mail) bool {
			t, err := time.Parse(time.RFC3339, m.Date)
			return err == nil && !t.Before(cutoff)
		}
		err := s.walkAccountFolders(ctx, w.acct, w.password, w.acct.ID, w.folders, scanFolderLimit, true, withinRetention, func(c *imapclient.Client, m Mail, body MailBody, folder string) bool {
			if body.HTML == "" {
				return true
			}
			for _, src := range extractImageSrcs(body.HTML) {
				if _, err := s.cachedOrFetchImage(src, true); err == nil {
					cacheMu.Lock()
					imagesCached++
					cacheMu.Unlock()
				}
			}
			return true
		}, func(done, _ int) {
			progress(foldersDone+done, totalFolders)
		})
		foldersDone += len(w.folders)
		if err != nil {
			return imagesCached, err
		}
	}
	s.db.Exec(`
		INSERT INTO owner_settings (owner_subject, image_backfill_completed_at) VALUES ($1, now())
		ON CONFLICT (owner_subject) DO UPDATE SET image_backfill_completed_at = excluded.image_backfill_completed_at`,
		ownerSubject,
	)
	return imagesCached, nil
}

// fetchedImage is what fetchAndVerifyImage returns and image_cache stores — verified
// (sniffed) content type plus the raw bytes.
type fetchedImage struct {
	ContentType string
	Bytes       []byte
}

// handleImageProxy fetches a remote image server-side and streams it back, so a mail's
// sender sees this server's IP/timing instead of the actual reader's — the privacy goal
// (hiding who/when/where you opened mail from) without the all-or-nothing tradeoff of
// either fully blocking images or fully trusting a sender's direct links. "Just block
// the tracking but still load images" can't be done by inspecting the image request
// itself (a tracking pixel looks identical to a real image at the HTTP level) — proxying
// is what every major mail provider actually does for this.
func (s *Store) handleImageProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	if raw == "" {
		http.Error(w, "u is required", http.StatusBadRequest)
		return
	}
	// mid is the client-visible mail ID (reader.js's proxyImageSrcs) — optional, only
	// sent when the client knows which mail this image belongs to. When present and
	// that mail currently carries the Spam tag, the image still loads (it's still
	// proxied, never raw), it just never gets written into the persistent cache.
	cacheable := true
	if mid := r.URL.Query().Get("mid"); mid != "" {
		var count int
		s.db.QueryRow(
			`SELECT count(*) FROM mails m
			 JOIN message_tags mt ON mt.message_id = m.message_id
			 JOIN tags t ON t.id = mt.tag_id
			 WHERE m.id = $1 AND t.name = 'Spam'`,
			mid,
		).Scan(&count)
		cacheable = count == 0
	}
	img, err := s.cachedOrFetchImage(raw, cacheable)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", img.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(img.Bytes)
}

func imageCacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// cachedOrFetchImage checks image_cache first — populated either by a previous live
// proxy request, or by prefetchMailImages below running ahead of time at sync — and
// only does a real fetch on a miss. cacheable controls only whether a *fresh* fetch
// gets written back to the cache — a cache hit is still served either way, since by
// definition that bytes-on-disk already exists regardless of who's asking now; this
// only stops spam-flagged mail's images from being the thing that *first* populates a
// cache entry, never raw-fetched directly (still goes through this same proxy).
func (s *Store) cachedOrFetchImage(rawURL string, cacheable bool) (fetchedImage, error) {
	key := imageCacheKey(rawURL)
	var img fetchedImage
	err := s.db.QueryRow("SELECT content_type, bytes FROM image_cache WHERE url_hash = $1", key).Scan(&img.ContentType, &img.Bytes)
	if err == nil {
		return img, nil
	}

	img, err = fetchAndVerifyImage(rawURL)
	if err != nil {
		return fetchedImage{}, err
	}
	if cacheable {
		s.db.Exec(
			"INSERT INTO image_cache (url_hash, content_type, bytes) VALUES ($1, $2, $3) ON CONFLICT (url_hash) DO NOTHING",
			key, img.ContentType, img.Bytes,
		)
	}
	return img, nil
}

// fetchAndVerifyImage does the actual network fetch. Since this fetches a URL the
// client (or a mail body) supplies, it's a classic SSRF shape: reject non-http(s)
// schemes and any hostname that resolves to a private/loopback/link-local address, so
// it can't be used to probe internal services or cloud metadata endpoints.
func fetchAndVerifyImage(rawURL string) (fetchedImage, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fetchedImage{}, fmt.Errorf("invalid url")
	}
	if err := rejectPrivateHost(u.Hostname()); err != nil {
		return fetchedImage{}, fmt.Errorf("blocked host")
	}

	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("invalid redirect scheme")
			}
			return rejectPrivateHost(req.URL.Hostname())
		},
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return fetchedImage{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sift image proxy)")
	resp, err := client.Do(req)
	if err != nil {
		return fetchedImage{}, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fetchedImage{}, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	// Don't trust the upstream-declared Content-Type alone — a malicious server can
	// claim "image/png" while actually serving anything. Sniff the real bytes (stdlib,
	// the same logic net/http itself uses) and reject anything that doesn't genuinely
	// sniff as a raster image, explicitly excluding SVG even if it did: SVG is XML and
	// can carry embedded script — the reader's sandboxed iframe already blocks script
	// execution, but there's no reason to serve it through here at all when nothing
	// downstream needs vector images for mail content.
	if strings.Contains(resp.Header.Get("Content-Type"), "svg") {
		return fetchedImage{}, fmt.Errorf("svg images aren't served by this proxy")
	}
	const maxImageBytes = 10 << 20 // 10MB — generous for a mail-embedded image, caps abuse
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return fetchedImage{}, err
	}
	sniffed := http.DetectContentType(body)
	if !strings.HasPrefix(sniffed, "image/") {
		return fetchedImage{}, fmt.Errorf("not a recognized image format")
	}
	return fetchedImage{ContentType: sniffed, Bytes: body}, nil
}

// prefetchMailImages fetches and caches every remote image in a freshly-synced batch
// of mail, regardless of whether any of it is ever opened — the same approach Apple
// Mail/Gmail use to decorrelate "an image was fetched" from "you actually read this",
// since a sender can otherwise infer roughly when you opened mail just from the
// proxy's own fetch timing even with the IP itself hidden. Runs as a background
// goroutine kicked off from syncAccount; best-effort throughout (a failed body fetch or
// blocked/broken image just means that one mail's images stay uncached — never worth
// failing or even logging loudly over, the live proxy path still works as a fallback).
func (s *Store) prefetchMailImages(acct dbAccount, password string, mails []Mail) {
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", acct.ID).Scan(&ownerSubject); err != nil {
		return
	}
	spamTagID, err := s.getOrCreateSpamTag(ownerSubject)
	if err != nil {
		log.Printf("get/create spam tag: %v", err)
		spamTagID = "" // scoreSpam treats "" as "skip history reinforcement" — still runs the rest
	} else {
		// Created proactively, not just inside the auto-apply branch below — in Review
		// mode nothing there ever runs (everything's only ever suggested, never
		// applied), which left no destination folder for autoMoveTaggedMail to use
		// once a suggestion eventually got accepted by hand.
		s.ensureSpamFolderRule(acct.ID, spamTagID)
	}
	mode := s.ownerSpamMode(ownerSubject)
	tagMode := s.ownerAutoTagMode(ownerSubject)

	for _, m := range mails {
		body, err := fetchMailBody(acct, password, m.Folder, m.UID)
		if err != nil {
			continue
		}
		// The inbox row's snippet preview — mailsFromFetch (imap.go) never had one
		// (envelope-only, no body fetch there), so every real mail's snippet sat
		// permanently blank. This is already fetching the full body for every new
		// mail anyway (spam scoring, image extraction below), so deriving one here is
		// free — no extra IMAP round trip.
		s.db.Exec("UPDATE mails SET snippet = $1 WHERE id = $2", snippetFromBody(body, 200), m.ID)
		bodyTokens := tokenizeBody(bodyPlainText(body))

		if m.MessageID != "" && m.SenderEmail != "" {
			// The live new-mail sync (evaluateNewMailForSmartTags) already ran this same
			// scoring with no body, so it can apply/suggest fast without waiting on this
			// background fetch — this re-run is what gives bodyProfileRatio (the tag's
			// own word profile) something to actually compare against for live mail, not
			// just scan-time mail. Existing/alreadySuggested guards inside it mean this
			// never double-logs whatever the fast pass already decided.
			s.evaluateOneMailForSmartTags(ownerSubject, acct.ID, tagMode, m, bodyTokens)
			score, reasons, spf, dkim, dmarc := s.scoreSpam(ownerSubject, spamTagID, m, body)
			// Stored for every scored mail, not just ones that clear the suggest
			// threshold — read back by the reader's bottom-of-mail diagnostic readout
			// (mails.go), so a low-scoring mail still has something to show.
			s.recordSpamFlags(m.MessageID, ownerSubject, m.SenderEmail, score, reasons, spf, dkim, dmarc)
			// This runs on every sync, for every mail in the batch — a mail that gets a
			// fresh UID without genuinely being new (auto-move, a folder rule, anything
			// else outside this sync) can reach this path more than once for the same
			// underlying message. Unlike scanAccountForSpam (which has had this guard all
			// along), this path had no "already decided" check at all — a mail the user
			// had explicitly dismissed got rescored and re-suggested on the very next
			// sync that happened to touch it.
			var alreadyDecided, alreadySuggested bool
			s.db.QueryRow(
				"SELECT EXISTS(SELECT 1 FROM message_tags WHERE message_id = $1 AND tag_id = $2) OR EXISTS(SELECT 1 FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'dismissed'), EXISTS(SELECT 1 FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested')",
				m.MessageID, spamTagID,
			).Scan(&alreadyDecided, &alreadySuggested)
			if alreadyDecided {
				continue
			}
			if alreadySuggested {
				// Same gap as scanAccountForSpam's own fix — a pending suggestion isn't a
				// decision, and this path rescoring the same mail lower (sender history
				// catching up, a temperror that resolved, anything else) means the original
				// suggestion no longer holds. This is the only other place mail actually
				// gets rescored after being suggested (besides an explicit "Scan for spam"
				// run), so it needs the identical retraction, not just the same guard.
				if score < spamSuggestScore {
					s.db.Exec(
						"DELETE FROM tag_history WHERE message_id = $1 AND tag_id = $2 AND status = 'suggested'",
						m.MessageID, spamTagID,
					)
				}
				continue
			}
			if score >= spamSuggestScore {
				status := "suggested"
				// Review means review — a high score never overrides the mode on its own.
				if mode == "full_auto" && score >= spamAutoApplyScore {
					status = "applied"
					s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", m.MessageID, spamTagID)
					s.ensureSpamFolderRule(acct.ID, spamTagID)
				}
				s.recordTagHistory(ownerSubject, acct.ID, m.MessageID, spamTagID, m.SenderEmail, m.Subject, "spam_engine", status, &score, tokenizeBody(bodyPlainText(body)))
				if status == "applied" {
					// Flagged mail shouldn't have its images warmed into the persistent
					// cache ahead of time — see imageproxy's cacheable handling for the
					// on-demand-view side of this same rule.
					continue
				}
			}
		}

		if body.HTML == "" {
			continue
		}
		for _, src := range extractImageSrcs(body.HTML) {
			if _, err := s.cachedOrFetchImage(src, true); err != nil {
				log.Printf("prefetch image %s: %v", src, err)
			}
		}
	}
}

// getOrCreateSpamTag returns the owner's "Spam" tag, creating it the first time it's
// needed — same get-or-create upsert handleCreateTag (tags.go) already uses, so it
// shows up in the Tag Manager like any other tag rather than being a hidden concept.
func (s *Store) getOrCreateSpamTag(ownerSubject string) (string, error) {
	var id string
	// instant_move = true on creation — spam shouldn't sit in inbox for the regular
	// auto_move_delay_days like an ordinary tag; autoMoveTaggedMail (smarttags.go)
	// already skips the delay entirely for any tag with this set, on its next
	// scheduled or manually-triggered run. Only set at creation (excluded.name in the
	// upsert leaves an already-existing tag's own instant_move alone, in case the user
	// deliberately turned it back off via the Tag Manager).
	err := s.db.QueryRow(`
		INSERT INTO tags (id, owner_subject, name, color, instant_move) VALUES ($1, $2, 'Spam', $3, true)
		ON CONFLICT (owner_subject, name) DO UPDATE SET name = excluded.name
		RETURNING id`,
		randomID(), ownerSubject, colorForName("Spam"),
	).Scan(&id)
	return id, err
}

// review (not full_auto) is the default — set in the 0027 migration — for the
// opposite reason regular tagging's own default leans review: a spam false positive
// burying real mail is worse than a missed spam-suggestion sitting in a queue.
func (s *Store) ownerSpamMode(ownerSubject string) string {
	var mode string
	if err := s.db.QueryRow("SELECT spam_mode FROM owner_settings WHERE owner_subject = $1", ownerSubject).Scan(&mode); err != nil {
		return "review"
	}
	return mode
}

// ensureSpamFolderRule wires the Spam tag to the account's Junk folder the first time
// it's needed, the same way a user could do by hand via the Tag Manager's destination
// picker — once this rule exists, the *existing* autoMoveTaggedMail (smarttags.go)
// already moves tagged-and-aged inbox mail there on its own; no new move code at all.
func (s *Store) ensureSpamFolderRule(accountID, spamTagID string) {
	var existing int
	s.db.QueryRow("SELECT count(*) FROM folder_tag_rules WHERE account_id = $1 AND tag_id = $2", accountID, spamTagID).Scan(&existing)
	if existing > 0 {
		return
	}
	_, _, junk, err := s.resolveFolders(accountID)
	if err != nil || junk == "" {
		return
	}
	s.db.Exec(
		"INSERT INTO folder_tag_rules (account_id, folder, tag_id) VALUES ($1, $2, $3) ON CONFLICT (account_id, folder) DO UPDATE SET tag_id = excluded.tag_id",
		accountID, junk, spamTagID,
	)
}

// extractImageSrcs walks parsed HTML for every <img src="http(s)://...">  — mirrors
// the client-side rewrite in reader.js's proxyImageSrcs, just server-side and read-only.
func extractImageSrcs(rawHTML string) []string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil
	}
	var srcs []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, attr := range n.Attr {
				if attr.Key == "src" && strings.HasPrefix(attr.Val, "http") {
					srcs = append(srcs, attr.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return srcs
}

// maybeCleanupImageCache opportunistically prunes old cached images — no real cron
// infra in this app (see syncAccount/autoMoveTaggedMail for the same pattern), so
// instead of a dedicated scheduler this just has a small random chance of running
// every time it's called from a real sync, which happens often enough on its own.
// image_cache has no owner_subject of its own (a single shared cache, not scoped per
// account) — this is a personal, single-user app, so just using whichever
// owner_settings row exists is fine; defaults to 90 days if none does yet.
func (s *Store) maybeCleanupImageCache() {
	if rand.Intn(50) != 0 { // ~2% of calls
		return
	}
	days := 90
	s.db.QueryRow("SELECT image_cache_retention_days FROM owner_settings LIMIT 1").Scan(&days)
	if _, err := s.db.Exec("DELETE FROM image_cache WHERE fetched_at < now() - ($1 * interval '1 day')", days); err != nil {
		log.Printf("cleanup image cache: %v", err)
	}
}

// rejectPrivateHost resolves hostname and rejects it if any resolved address is
// private, loopback, link-local, or otherwise not a normal public address. This is the
// only DNS lookup Sift itself performs (every auth check elsewhere — SPF/DKIM/DMARC —
// reads a verdict the mail server already computed, nothing here to retry) — retried
// up to 3 times since a single resolver hiccup shouldn't permanently block an image
// that would resolve fine a moment later.
func rejectPrivateHost(hostname string) error {
	var ips []net.IP
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		ips, err = net.LookupIP(hostname)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("resolve %s: %w", hostname, err)
	}
	for _, ip := range ips {
		if isPrivateOrSpecialIP(ip) {
			return fmt.Errorf("%s resolves to a non-public address", hostname)
		}
	}
	return nil
}

func isPrivateOrSpecialIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
