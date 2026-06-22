package api

import (
	"context"
	"log"
)

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
		mails, err := fetchFolderMail(acct, password, folder, limit, 0)
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
