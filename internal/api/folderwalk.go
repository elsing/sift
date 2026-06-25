package api

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// withTimeout bounds a blocking call that has no timeout/context support of its own —
// every IMAP operation in this codebase, notably (dialIMAP/imapclient have no deadline
// or context awareness at all). A stalled connection — a network blip, a server that
// stops responding mid-command — would otherwise hang the calling goroutine forever,
// which is exactly what froze a image-cache backfill or tag scan partway through with
// no way to recover except restarting the whole server. The orphaned goroutine on a
// real timeout leaks (Go has no way to forcibly abort a blocked network read), but
// that's a single goroutine sitting on a connection that'll eventually error out on its
// own — much better than the entire operation never progressing again.
func withTimeout[T any](d time.Duration, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(d):
		var zero T
		return zero, fmt.Errorf("timed out after %s", d)
	}
}

const imapOpTimeout = 25 * time.Second

// messageProcessingConcurrency bounds how many messages from one fetched page get
// onMail-processed at once — a second, smaller tier of concurrency layered on top of
// maxConcurrentFolderSearches' per-folder one. Fetching a page in one batched IMAP
// command (fetchMailBodiesOnConn) already collapsed the network round trips; this is
// what stops the actual scoring/parsing work afterwards from being one-at-a-time too.
// A flat default, not dynamic — sizing this off live server stats would be a lot of
// machinery for a number that mostly just needs to not be 1 or 200.
const messageProcessingConcurrency = 10

// walkAccountFolders pages through every one of the given folders in full — not just
// the most recent `limit` (page size) mails — caching everything it sees locally (so
// any of it has a real mails.id other features can resolve later) and calling onMail
// for each one as its folder finishes. Shared by the Smart Tagging scanner, the spam
// scanner, and the image-cache backfill — all three need "go through every folder of
// an account, see what's there," just doing something different with what they find.
//
// onMail returns whether to keep paging further back in the current folder — true for
// "process everything" (Smart Tagging/spam scan want the genuine full history), false
// once a caller's own cutoff (e.g. the image backfill's retention-day window) means
// nothing older in this folder is relevant either, since folders are date-ordered
// newest-first. Capping at `limit` per folder used to be the only option, silently
// scanning a fraction of a folder with no sign anything was skipped — a 500+-message
// Junk folder only ever got its newest 200 checked for spam, no matter how many times
// "Scan for spam" ran.
//
// onMail can be called concurrently — both across folders (maxConcurrentFolderSearches)
// and across messages within one folder's page (messageProcessingConcurrency). It is
// NOT serialized for callers anymore: any shared state an onMail closure touches (a
// counter, a slice append, an SSE writer) needs its own synchronization.
//
// onMail gets each message's body pre-fetched when needBodies is true — the whole
// page's worth in one IMAP command (fetchMailBodiesOnConn), not one fetch per
// message. A page of 200 used to mean 200 round trips here; the actual bytes
// transferred are the same either way, but round-trip latency (not transfer time) is
// what dominated a scan's total time, and batching collapses 200 of those into 1.
// body is the zero value when needBodies is false, or when this particular message's
// body fetch failed (best-effort — one bad message in a batch doesn't lose the rest).
//
// filter, if non-nil, is a cheap pre-check using only the envelope data already in
// hand (no IO) — the image backfill's retention-day cutoff, in particular. The first
// message filter rejects stops the walk for the *rest of this folder*, not just that
// message: folders are date-ordered newest-first, so once one message is too old,
// every later (older) one in the same folder is too, and skipping them before they'd
// otherwise cost a wasted batch-fetch slot is the whole point of checking this early
// rather than after fetching the body. nil means "every message qualifies" (the spam
// and tag scanners, which deliberately score everything regardless of age).
//
// onMail can still independently return false to stop early for any other reason —
// filter and onMail's return value are two separate ways to say "stop here," kept
// separate because filter runs before the batch fetch (saving the fetch) and onMail's
// return is checked after (for reasons that need to see the actual message/body).
func (s *Store) walkAccountFolders(
	ctx context.Context, acct dbAccount, password, accountID string, folders []string, limit int, needBodies bool,
	filter func(m Mail) bool,
	onMail func(c *imapclient.Client, m Mail, body MailBody, folder string) bool, onProgress func(done, total int),
) error {
	var mu sync.Mutex // guards only the folder-done progress counter below
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentFolderSearches)
	done := 0

	// A full-history walk (filter == nil — the Smart Tagging/spam scanners) of just one
	// or two folders left most of the 8-connection budget idle: one folder, one
	// connection, no matter how large that folder is — a 2400-message Junk folder still
	// only ever got walked by a single connection paging 200 at a time. lanes splits
	// each such folder's own pagination across several connections instead, sized so a
	// lone huge folder still gets every connection in the budget. Skipped for a filtered
	// walk (the image backfill): that relies on one connection seeing every message
	// strictly newest-to-oldest to know when to stop early at the retention cutoff —
	// splitting into out-of-date-order lanes would break that.
	lanes := 1
	var folderCounts map[string]int
	if filter == nil && len(folders) > 0 {
		lanes = maxConcurrentFolderSearches / len(folders)
		if lanes < 1 {
			lanes = 1
		}
		if lanes > 1 {
			folderCounts = folderMessageCounts(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, folders)
		}
	}

	// Folders are walked concurrently — same reasoning and same cap as search.go's
	// maxConcurrentFolderSearches (a stateful IMAP connection can only be SELECTed on
	// one mailbox at a time, so real concurrency needs one connection per concurrent
	// folder, bounded so a many-folder mailbox doesn't trip a provider's connection
	// limit). sem is shared across both folders and, when lanes > 1, the lanes within
	// one folder — the total number of connections open at once never exceeds the cap
	// regardless of how that budget gets split.
	for _, folder := range folders {
		if ctx.Err() != nil {
			break // client closed the connection (e.g. tapped Cancel) — stop starting new folders on a scan nobody's waiting for
		}
		folderLanes := lanes
		if folderLanes > 1 && folderCounts[folder] == 0 {
			folderLanes = 1 // empty, or the STATUS lookup failed — one connection is enough either way
		}
		if folderLanes <= 1 {
			wg.Add(1)
			go func(folder string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				c, err := dialIMAP(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
				if err != nil {
					log.Printf("walk %s/%s: connect: %v", acct.Email, folder, err)
					return
				}
				defer c.Close()
				s.walkOneFolder(ctx, c, accountID, acct.Email, folder, limit, needBodies, filter, onMail)
				mu.Lock()
				done++
				if onProgress != nil {
					onProgress(done, len(folders))
				}
				mu.Unlock()
			}(folder)
			continue
		}

		// Partition this folder's sequence-number space into folderLanes contiguous
		// slices up front (known from the STATUS pass above) — each lane then walks its
		// own slice independently, on its own connection.
		total := uint32(folderCounts[folder])
		chunk := total / uint32(folderLanes)
		if chunk == 0 {
			chunk = 1
		}
		var laneWg sync.WaitGroup
		start := uint32(1)
		for lane := 0; lane < folderLanes && start <= total; lane++ {
			end := start + chunk - 1
			if lane == folderLanes-1 || end > total {
				end = total // last lane (or a small remainder) absorbs whatever's left
			}
			wg.Add(1)
			laneWg.Add(1)
			go func(folder string, start, end uint32) {
				defer wg.Done()
				defer laneWg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				c, err := dialIMAP(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
				if err != nil {
					log.Printf("walk %s/%s (%d-%d): connect: %v", acct.Email, folder, start, end, err)
					return
				}
				defer c.Close()
				if _, err := withTimeout(imapOpTimeout, func() (*imap.SelectData, error) { return c.Select(folder, nil).Wait() }); err != nil {
					return
				}
				s.walkFolderRange(ctx, c, accountID, folder, start, end, limit, needBodies, onMail)
			}(folder, start, end)
			start = end + 1
		}
		wg.Add(1)
		go func(folder string) {
			defer wg.Done()
			laneWg.Wait()
			mu.Lock()
			done++
			if onProgress != nil {
				onProgress(done, len(folders))
			}
			mu.Unlock()
		}(folder)
	}
	wg.Wait()
	return ctx.Err()
}

// walkFolderRange walks one sequence-number slice [seqStart, seqEnd] of an
// already-selected folder, in limit-sized sub-batches — the parallel-lane counterpart
// to walkOneFolder's UID-cursor pagination (see walkAccountFolders' own comment on
// when this is used instead). No filter/early-stop here: lanes only ever cover a
// full-history walk, which never has one.
func (s *Store) walkFolderRange(
	ctx context.Context, c *imapclient.Client, accountID, folder string, seqStart, seqEnd uint32, limit int, needBodies bool,
	onMail func(c *imapclient.Client, m Mail, body MailBody, folder string) bool,
) {
	for start := seqStart; start <= seqEnd; {
		if ctx.Err() != nil {
			return
		}
		end := start + uint32(limit) - 1
		if end > seqEnd {
			end = seqEnd
		}
		mails, err := withTimeout(imapOpTimeout, func() ([]Mail, error) {
			return fetchFolderMailRange(c, accountID, folder, start, end)
		})
		if err != nil {
			return
		}
		for i := range mails {
			mails[i].AccountID = accountID
		}
		if err := s.upsertMails(accountID, mails); err != nil {
			log.Printf("cache walked mail range %s %d-%d: %v", folder, start, end, err)
		}

		var bodies map[uint32]MailBody
		if needBodies && len(mails) > 0 {
			uids := make([]uint32, len(mails))
			for i, m := range mails {
				uids[i] = m.UID
			}
			bodies = fetchMailBodiesOnConn(c, uids)
		}
		processMailBatch(c, mails, bodies, folder, onMail)
		start = end + 1
	}
}

// processMailBatch runs onMail across mails (with their already-fetched bodies)
// messageProcessingConcurrency at a time, returning whether onMail returned false for
// any of them — shared between walkOneFolder's per-page loop and walkFolderRange's
// per-lane loop above, so this concurrency tier is written exactly once.
func processMailBatch(
	c *imapclient.Client, mails []Mail, bodies map[uint32]MailBody, folder string,
	onMail func(c *imapclient.Client, m Mail, body MailBody, folder string) bool,
) bool {
	var (
		stopMu  sync.Mutex
		allTrue = true
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, messageProcessingConcurrency)
	for _, m := range mails {
		wg.Add(1)
		sem <- struct{}{}
		go func(m Mail) {
			defer wg.Done()
			defer func() { <-sem }()
			if !onMail(c, m, bodies[m.UID], folder) {
				stopMu.Lock()
				allTrue = false
				stopMu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	return allTrue
}

// walkOneFolder pages through a single folder on the given (already-connected)
// client, oldest page ending the loop either when the folder's exhausted or onMail
// says to stop. Split out from walkAccountFolders purely so each folder gets its own
// mbox SelectData fresh (re-selecting mid-walk on the same connection, rather than
// needing a separate connection per folder).
func (s *Store) walkOneFolder(
	ctx context.Context, c *imapclient.Client, accountID, acctEmail, folder string, limit int, needBodies bool,
	filter func(m Mail) bool, onMail func(c *imapclient.Client, m Mail, body MailBody, folder string) bool,
) {
	mbox, err := withTimeout(imapOpTimeout, func() (*imap.SelectData, error) {
		return c.Select(folder, nil).Wait()
	})
	if err != nil {
		return // unselectable/empty folder — skip, not fatal to the rest of the walk
	}

	var beforeUID uint32
	for {
		if ctx.Err() != nil {
			return
		}
		mails, err := withTimeout(imapOpTimeout, func() ([]Mail, error) {
			return fetchFolderMailPage(c, accountID, mbox, folder, limit, beforeUID)
		})
		if err != nil || len(mails) == 0 {
			return
		}
		for i := range mails {
			mails[i].AccountID = accountID
		}
		// Without this, scanned mail was never cached locally at all — there was no
		// mails.id to resolve, so a scan-derived suggestion (or, for the image
		// backfill, a later "Show images" tap) could never be tied back to it.
		if err := s.upsertMails(accountID, mails); err != nil {
			log.Printf("cache walked mail %s/%s: %v", acctEmail, folder, err)
		}

		// filter trims the page down to a prefix *before* fetching any bodies — see
		// walkAccountFolders' own comment on why a rejection here ends the whole
		// folder, not just that one message.
		filteredAll := true
		if filter != nil {
			for i, m := range mails {
				if !filter(m) {
					mails = mails[:i]
					filteredAll = false
					break
				}
			}
		}

		var bodies map[uint32]MailBody
		if needBodies && len(mails) > 0 {
			uids := make([]uint32, len(mails))
			for i, m := range mails {
				uids[i] = m.UID
			}
			bodies = fetchMailBodiesOnConn(c, uids)
		}

		for _, m := range mails {
			if m.UID > 0 && (beforeUID == 0 || m.UID < beforeUID) {
				beforeUID = m.UID
			}
		}
		// The page's messages are processed messageProcessingConcurrency at a time, not
		// one at a time — bodies are already in hand (fetched as a single batch above),
		// so this is purely about letting the scoring/parsing work itself overlap rather
		// than queueing behind each other.
		keepGoing := processMailBatch(c, mails, bodies, folder, onMail)
		if !keepGoing || !filteredAll || len(mails) < limit {
			return // onMail or filter said stop, or that was the last (oldest, partial) page anyway
		}
	}
}
