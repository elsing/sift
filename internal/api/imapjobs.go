package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	imap "github.com/emersion/go-imap/v2"
)

// imapJobPayload carries the IMAP-side parameters for a queued operation.
// Fields used depend on kind: move/archive/delete use UID+Folder(+DestFolder);
// set-read/set-unread use UID+Folder.
type imapJobPayload struct {
	UID        uint32 `json:"uid,omitempty"`
	Folder     string `json:"folder,omitempty"`
	DestFolder string `json:"destFolder,omitempty"`
}

// enqueueImapJob inserts a new pending IMAP job. Fire-and-forget: errors are logged,
// not returned — the caller has already responded to the HTTP client.
func (s *Store) enqueueImapJob(accountID, kind string, payload imapJobPayload) {
	b, _ := json.Marshal(payload)
	if _, err := s.db.Exec(
		"INSERT INTO imap_jobs (id, account_id, kind, payload) VALUES ($1, $2, $3, $4)",
		randomID(), accountID, kind, string(b),
	); err != nil {
		log.Printf("enqueue imap job %s: %v", kind, err)
	}
}

// runImapJobWorker drains the imap_jobs queue until ctx is cancelled.
// Processes one job at a time; when idle sleeps 2 s between polls.
func (s *Store) runImapJobWorker(ctx context.Context) {
	for {
		if !s.processNextImapJob() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		} else {
			// processed one — check immediately for the next
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}

func (s *Store) processNextImapJob() bool {
	var id, accountID, kind string
	var payloadBytes []byte
	err := s.db.QueryRow(
		"SELECT id, account_id, kind, payload FROM imap_jobs WHERE status = 'pending' ORDER BY created_at LIMIT 1",
	).Scan(&id, &accountID, &kind, &payloadBytes)
	if err != nil {
		return false // nothing pending (or db error — don't spin)
	}

	var payload imapJobPayload
	json.Unmarshal(payloadBytes, &payload) //nolint:errcheck — bad JSON would just leave zero values

	if err := s.executeImapJob(accountID, kind, payload); err != nil {
		log.Printf("imap job %s (%s): %v", id, kind, err)
		s.db.Exec("UPDATE imap_jobs SET status = 'error', error = $1, updated_at = now() WHERE id = $2", err.Error(), id) //nolint:errcheck
	} else {
		s.db.Exec("UPDATE imap_jobs SET status = 'done', updated_at = now() WHERE id = $1", id) //nolint:errcheck
	}
	return true
}

func (s *Store) executeImapJob(accountID, kind string, payload imapJobPayload) error {
	if payload.UID == 0 {
		return nil // mock mail or no IMAP UID — nothing to sync
	}
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return err
	}
	switch kind {
	case "move":
		if payload.Folder == payload.DestFolder {
			return nil
		}
		return moveMail(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, payload.UID, payload.Folder, payload.DestFolder)
	case "archive", "delete":
		archive, trash, _, err := s.resolveFolders(accountID)
		if err != nil {
			return err
		}
		dest := archive
		if kind == "delete" {
			dest = trash
		}
		if dest == "" {
			return fmt.Errorf("no %s folder found for account", kind)
		}
		return moveMail(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, payload.UID, payload.Folder, dest)
	case "set-read":
		return setMailSeen(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, payload.UID, payload.Folder, true)
	case "set-unread":
		return setMailSeen(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, payload.UID, payload.Folder, false)
	}
	return fmt.Errorf("unknown imap job kind %q", kind)
}

// setMailSeen adds or removes the IMAP \Seen flag on a single message.
func setMailSeen(host string, port int, username, password, oauthProvider string, uid uint32, folder string, seen bool) error {
	c, err := dialIMAP(host, port, username, password, oauthProvider)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		return fmt.Errorf("select %s: %w", folder, err)
	}
	op := imap.StoreFlagsAdd
	if !seen {
		op = imap.StoreFlagsDel
	}
	storeFlags := imap.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagSeen}, Silent: true}
	if err := c.Store(imap.UIDSetNum(imap.UID(uid)), &storeFlags, nil).Close(); err != nil {
		return fmt.Errorf("store flags: %w", err)
	}
	return nil
}
