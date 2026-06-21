package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	imap "github.com/emersion/go-imap/v2"
)

type dbAccount struct {
	ID       string
	Email    string
	Host     string
	Port     int
	Username string
}

type accountOut struct {
	ID              string   `json:"id"`
	Email           string   `json:"email"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	LastSyncError   string   `json:"lastSyncError,omitempty"`
	ExpandedFolders []string `json:"expandedFolders"`
}

type addAccountReq struct {
	Email    string `json:"email"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// AccountsRoutes registers account management endpoints. ownerSubject extracts the
// logged-in user's subject from the request (set by the auth middleware's context).
func (s *Store) AccountsRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/accounts", func(w http.ResponseWriter, r *http.Request) {
		s.handleListAccounts(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/accounts", func(w http.ResponseWriter, r *http.Request) {
		s.handleAddAccount(w, r, ownerSubject(r))
	})
	mux.HandleFunc("DELETE /api/accounts/{id}", s.handleDeleteAccount)
	mux.HandleFunc("POST /api/accounts/{id}/sync", s.handleSyncAccount)
	mux.HandleFunc("GET /api/accounts/{id}/folders", s.handleListFolders)
	mux.HandleFunc("GET /api/accounts/{id}/folder-mails", s.handleFolderMails)
	mux.HandleFunc("PUT /api/accounts/{id}/expanded-folders", s.handleSetExpandedFolders)
}

// isDryRun reports whether the request asked to simulate a mutation rather than perform it.
func isDryRun(r *http.Request) bool {
	return r.Header.Get("X-Dry-Run") == "1"
}

// loadAccountCreds fetches and decrypts the IMAP credentials for an account.
func (s *Store) loadAccountCreds(id string) (dbAccount, string, error) {
	var acct dbAccount
	var encPassword []byte
	err := s.db.QueryRow(
		"SELECT id, email, host, port, username, password_enc FROM accounts WHERE id = $1", id,
	).Scan(&acct.ID, &acct.Email, &acct.Host, &acct.Port, &acct.Username, &encPassword)
	if err != nil {
		return acct, "", fmt.Errorf("load account: %w", err)
	}
	password, err := s.crypto.decrypt(encPassword)
	if err != nil {
		return acct, "", fmt.Errorf("decrypt password: %w", err)
	}
	return acct, password, nil
}

func (s *Store) handleListAccounts(w http.ResponseWriter, r *http.Request, owner string) {
	rows, err := s.db.Query(
		"SELECT id, email, host, port, coalesce(last_sync_error, ''), expanded_folders FROM accounts WHERE owner_subject = $1 ORDER BY created_at",
		owner,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []accountOut{}
	for rows.Next() {
		var a accountOut
		var expandedJSON string
		if err := rows.Scan(&a.ID, &a.Email, &a.Host, &a.Port, &a.LastSyncError, &expandedJSON); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.Unmarshal([]byte(expandedJSON), &a.ExpandedFolders)
		out = append(out, a)
	}
	writeJSON(w, out)
}

func (s *Store) handleSetExpandedFolders(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	encoded, err := json.Marshal(req.Paths)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Exec("UPDATE accounts SET expanded_folders = $1 WHERE id = $2", string(encoded), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleAddAccount(w http.ResponseWriter, r *http.Request, owner string) {
	var req addAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.Port == 0 || req.Username == "" || req.Password == "" || req.Email == "" {
		http.Error(w, "email, host, port, username and password are all required", http.StatusBadRequest)
		return
	}

	if err := testIMAPLogin(req.Host, req.Port, req.Username, req.Password); err != nil {
		log.Printf("add account %s: imap login failed: %v", req.Email, err)
		http.Error(w, "couldn't log in to that account: "+err.Error(), http.StatusBadRequest)
		return
	}

	encPassword, err := s.crypto.encrypt(req.Password)
	if err != nil {
		log.Printf("add account %s: encrypt password: %v", req.Email, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	id := randomID()
	_, err = s.db.Exec(
		"INSERT INTO accounts (id, owner_subject, email, host, port, username, password_enc) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		id, owner, req.Email, req.Host, req.Port, req.Username, encPassword,
	)
	if err != nil {
		log.Printf("add account %s: insert: %v", req.Email, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := accountOut{ID: id, Email: req.Email, Host: req.Host, Port: req.Port}
	if _, err := s.syncAccount(id); err != nil {
		// account is saved; record the sync failure instead of rolling back, surfaced via lastSyncError
		log.Printf("add account %s: initial sync failed: %v", req.Email, err)
		out.LastSyncError = err.Error()
	}
	s.watchAccount(id)
	writeJSON(w, out)
}

func (s *Store) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if isDryRun(r) {
		log.Printf("[dry-run] would delete account %s", id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := s.db.Exec("DELETE FROM accounts WHERE id = $1", id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.stopWatching(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleListFolders(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	names, delim, err := listFolders(acct.Host, acct.Port, acct.Username, password)
	if err != nil {
		log.Printf("list folders %s: %v", acct.Email, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, buildFolderTree(names, delim))
}

func (s *Store) handleFolderMails(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	folder := r.URL.Query().Get("path")
	if folder == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	mails, err := fetchFolderMail(acct, password, folder, pageSize)
	if err != nil {
		log.Printf("folder mails %s/%s: %v", acct.Email, folder, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// persist so archive/delete/move/read endpoints (which look mail up by id) work
	// the same inside a folder view as they do in the inbox.
	if err := s.upsertMails(id, mails); err != nil {
		log.Printf("cache folder mails %s/%s: %v", acct.Email, folder, err)
	}
	writeJSON(w, mails)
}

func (s *Store) handleSyncAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.syncAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	mails, err := s.list(0, pageSize, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, mails)
}

// syncAccount fetches recent mail for one account and upserts it into the mails table.
// Returns the mails IMAP actually reported this round (newest-first by UID is not
// guaranteed; callers that need "what's new" should look at UID, not slice order).
func (s *Store) syncAccount(accountID string) ([]Mail, error) {
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return nil, err
	}

	mails, oldestUID, err := syncIMAP(acct, password)
	if err != nil {
		syncErr := fmt.Errorf("imap sync: %w", err)
		s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", syncErr.Error(), accountID)
		return nil, syncErr
	}

	if err := s.upsertMails(accountID, mails); err != nil {
		s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", err.Error(), accountID)
		return nil, err
	}
	// only move the backfill boundary down (older); a regular sync's window can be
	// newer than what backfill has already reached further back in history.
	if oldestUID > 0 {
		s.db.Exec(
			"UPDATE accounts SET last_sync_error = NULL, oldest_synced_uid = LEAST(coalesce(oldest_synced_uid, $1), $1) WHERE id = $2",
			int64(oldestUID), accountID,
		)
	} else {
		s.db.Exec("UPDATE accounts SET last_sync_error = NULL WHERE id = $1", accountID)
	}
	return mails, nil
}

// upsertMails inserts/updates mails for an account inside a single transaction.
func (s *Store) upsertMails(accountID string, mails []Mail) error {
	if len(mails) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range mails {
		sentAt, _ := time.Parse(time.RFC3339, m.Date)
		_, err := tx.Exec(`
			INSERT INTO mails (id, account_id, sender, sender_email, subject, snippet, time, unread, uid, folder, sent_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (id) DO UPDATE SET
				sender = excluded.sender, sender_email = excluded.sender_email, subject = excluded.subject,
				snippet = excluded.snippet, time = excluded.time, unread = excluded.unread,
				uid = excluded.uid, folder = excluded.folder, sent_at = excluded.sent_at`,
			m.ID, accountID, m.Sender, m.SenderEmail, m.Subject, m.Snippet, m.Time, m.Unread, m.UID, m.Folder, sentAt,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// resolveFolders returns the account's Archive/Trash mailbox names, detecting and
// caching them on first use so we don't re-run a full folder LIST on every swipe.
// Either value can be "" if the server has no such folder.
func (s *Store) resolveFolders(accountID string) (archive, trash string, err error) {
	var archiveCol, trashCol *string
	err = s.db.QueryRow(
		"SELECT archive_folder, trash_folder FROM accounts WHERE id = $1", accountID,
	).Scan(&archiveCol, &trashCol)
	if err != nil {
		return "", "", err
	}
	if archiveCol != nil && trashCol != nil {
		return *archiveCol, *trashCol, nil
	}

	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return "", "", err
	}
	archive, trash, err = detectSpecialUseFolders(acct.Host, acct.Port, acct.Username, password)
	if err != nil {
		return "", "", err
	}
	s.db.Exec("UPDATE accounts SET archive_folder = $1, trash_folder = $2 WHERE id = $3", archive, trash, accountID)
	return archive, trash, nil
}

// moveMailToFolder loads a mail's account + UID + source folder and moves it to
// destFolder on the real IMAP server.
func (s *Store) moveMailToFolder(mailID, destFolder string) error {
	var accountID *string
	var uid *int64
	var sourceFolder string
	err := s.db.QueryRow("SELECT account_id, uid, folder FROM mails WHERE id = $1", mailID).Scan(&accountID, &uid, &sourceFolder)
	if err != nil {
		return err
	}
	if accountID == nil || uid == nil {
		return fmt.Errorf("mock mail has no real mailbox to move it in")
	}
	acct, password, err := s.loadAccountCreds(*accountID)
	if err != nil {
		return err
	}
	return moveMail(acct.Host, acct.Port, acct.Username, password, uint32(*uid), sourceFolder, destFolder)
}

// archiveOrDeleteOnServer moves a mail to the account's Archive/Trash folder for the
// given action. Mock mail (no account_id) has nothing real to move and is a no-op.
func (s *Store) archiveOrDeleteOnServer(mailID, action string) error {
	var accountID *string
	if err := s.db.QueryRow("SELECT account_id FROM mails WHERE id = $1", mailID).Scan(&accountID); err != nil {
		return err
	}
	if accountID == nil {
		return nil
	}

	archive, trash, err := s.resolveFolders(*accountID)
	if err != nil {
		return fmt.Errorf("resolve folders: %w", err)
	}

	var dest string
	switch action {
	case "archive":
		dest = archive
		if dest == "" {
			return fmt.Errorf("no Archive folder found on this account")
		}
	case "delete":
		dest = trash
		if dest == "" {
			return fmt.Errorf("no Trash folder found on this account")
		}
	}
	return s.moveMailToFolder(mailID, dest)
}

// backfillAccount pulls one more batch of older mail for an account, continuing from
// wherever the previous sync/backfill left off. Returns how many mails were added.
func (s *Store) backfillAccount(accountID string, batchSize int) (int, error) {
	var oldestSyncedUID *int64
	if err := s.db.QueryRow("SELECT oldest_synced_uid FROM accounts WHERE id = $1", accountID).Scan(&oldestSyncedUID); err != nil {
		return 0, fmt.Errorf("load boundary: %w", err)
	}
	if oldestSyncedUID == nil {
		return 0, nil // hasn't completed a regular sync yet; nothing to continue from
	}

	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return 0, err
	}

	mails, newOldestUID, err := backfillIMAP(acct, password, imap.UID(*oldestSyncedUID), batchSize)
	if err != nil {
		return 0, fmt.Errorf("imap backfill: %w", err)
	}
	if len(mails) == 0 {
		// nothing older than the current boundary; mark exhausted so we stop retrying
		s.db.Exec("UPDATE accounts SET oldest_synced_uid = 1 WHERE id = $1", accountID)
		return 0, nil
	}

	if err := s.upsertMails(accountID, mails); err != nil {
		return 0, err
	}
	s.db.Exec("UPDATE accounts SET oldest_synced_uid = $1 WHERE id = $2", int64(newOldestUID), accountID)
	return len(mails), nil
}

// backfillAllAccounts tops up every account that hasn't yet reached the start of its
// mailbox, so the inbox isn't capped at the most recent syncCount messages forever.
func (s *Store) backfillAllAccounts(batchSize int) (int, error) {
	rows, err := s.db.Query("SELECT id FROM accounts WHERE oldest_synced_uid IS NULL OR oldest_synced_uid > 1")
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	total := 0
	for _, id := range ids {
		n, err := s.backfillAccount(id, batchSize)
		if err != nil {
			log.Printf("backfill account %s: %v", id, err)
			continue
		}
		total += n
	}
	return total, nil
}

func (s *Store) syncAllAccounts() error {
	rows, err := s.db.Query("SELECT id FROM accounts")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		if _, err := s.syncAccount(id); err != nil {
			return fmt.Errorf("sync account %s: %w", id, err)
		}
	}
	return nil
}

func (s *Store) hasAccounts() (bool, error) {
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM accounts").Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
