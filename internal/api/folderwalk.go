package api

import (
	"context"
	"fmt"
	"log"
	"time"
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

// walkAccountFolders fetches up to `limit` recent mails from each of the given
// folders, caching them locally (so anything found has a real mails.id other features
// can resolve later) and calling onMail for each one as its folder finishes. Shared by
// the Smart Tagging scanner and the image-cache backfill — both need "go through every
// folder of an account, see what's there," just doing something different with what
// they find.
func (s *Store) walkAccountFolders(
	ctx context.Context, acct dbAccount, password, accountID string, folders []string, limit int,
	onMail func(m Mail, folder string), onProgress func(done, total int),
) error {
	for i, folder := range folders {
		if ctx.Err() != nil {
			return ctx.Err() // client closed the connection (e.g. tapped Cancel) — stop burning IMAP round trips on a scan nobody's waiting for
		}
		mails, err := withTimeout(imapOpTimeout, func() ([]Mail, error) {
			return fetchFolderMail(acct, password, folder, limit, 0)
		})
		if err == nil {
			// Without this, scanned mail was never cached locally at all — there was no
			// mails.id to resolve, so a scan-derived suggestion (or, for the image
			// backfill, a later "Show images" tap) could never be tied back to it.
			for i := range mails {
				mails[i].AccountID = accountID
			}
			if err := s.upsertMails(accountID, mails); err != nil {
				log.Printf("cache walked mail %s/%s: %v", acct.Email, folder, err)
			}
			for _, m := range mails {
				onMail(m, folder)
			}
		} // unselectable/empty folder — skip, not fatal to the rest of the walk
		if onProgress != nil {
			onProgress(i+1, len(folders))
		}
	}
	return nil
}
