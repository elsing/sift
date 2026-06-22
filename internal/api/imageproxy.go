package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ImageCacheRoutes registers the manual "backfill the last N days now" trigger —
// everything else (live proxying, prefetching new mail, cleanup) needs no client-facing
// endpoint of its own.
func (s *Store) ImageCacheRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/image-cache/backfill", func(w http.ResponseWriter, r *http.Request) {
		s.handleBackfillImageCache(w, r, ownerSubject(r))
	})
}

// handleBackfillImageCache streams progress over SSE the same way handleScanTags/
// handleSearch do — walking every already-cached mail within the retention window and
// fetching its images is a multi-IMAP-round-trip operation that can take a while.
func (s *Store) handleBackfillImageCache(w http.ResponseWriter, r *http.Request, owner string) {
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

	cached, err := s.backfillImageCache(r.Context(), owner, func(done, total int) {
		sendEvent("progress", map[string]int{"done": done, "total": total})
	})
	if err != nil {
		sendEvent("error", map[string]string{"message": err.Error()})
		return
	}
	sendEvent("complete", map[string]int{"imagesCached": cached})
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
		_, trashFolder, _ := detectSpecialUseFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
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
	foldersDone := 0
	for _, w := range work {
		err := s.walkAccountFolders(ctx, w.acct, w.password, w.acct.ID, w.folders, scanFolderLimit, func(m Mail, folder string) {
			t, err := time.Parse(time.RFC3339, m.Date)
			if err != nil || t.Before(cutoff) {
				return
			}
			body, err := fetchMailBody(w.acct, w.password, folder, m.UID)
			if err != nil || body.HTML == "" {
				return
			}
			for _, src := range extractImageSrcs(body.HTML) {
				if _, err := s.cachedOrFetchImage(src); err == nil {
					imagesCached++
				}
			}
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
	img, err := s.cachedOrFetchImage(raw)
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
// only does a real fetch on a miss.
func (s *Store) cachedOrFetchImage(rawURL string) (fetchedImage, error) {
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
	s.db.Exec(
		"INSERT INTO image_cache (url_hash, content_type, bytes) VALUES ($1, $2, $3) ON CONFLICT (url_hash) DO NOTHING",
		key, img.ContentType, img.Bytes,
	)
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
	for _, m := range mails {
		body, err := fetchMailBody(acct, password, m.Folder, m.UID)
		if err != nil || body.HTML == "" {
			continue
		}
		for _, src := range extractImageSrcs(body.HTML) {
			if _, err := s.cachedOrFetchImage(src); err != nil {
				log.Printf("prefetch image %s: %v", src, err)
			}
		}
	}
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
// private, loopback, link-local, or otherwise not a normal public address.
func rejectPrivateHost(hostname string) error {
	ips, err := net.LookupIP(hostname)
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
