package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

type dbAccount struct {
	ID       string
	Email    string
	Host     string
	Port     int
	Username string
}

type accountOut struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	LastSyncError string `json:"lastSyncError,omitempty"`
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
	rows, err := s.db.Query("SELECT id, email, host, port, coalesce(last_sync_error, '') FROM accounts WHERE owner_subject = $1 ORDER BY created_at", owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []accountOut{}
	for rows.Next() {
		var a accountOut
		if err := rows.Scan(&a.ID, &a.Email, &a.Host, &a.Port, &a.LastSyncError); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, a)
	}
	writeJSON(w, out)
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
	if err := s.syncAccount(id); err != nil {
		// account is saved; record the sync failure instead of rolling back, surfaced via lastSyncError
		log.Printf("add account %s: initial sync failed: %v", req.Email, err)
		out.LastSyncError = err.Error()
	}
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

func (s *Store) handleSyncAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.syncAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	mails, err := s.list()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, mails)
}

// syncAccount fetches recent mail for one account and upserts it into the mails table.
func (s *Store) syncAccount(accountID string) error {
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return err
	}

	mails, err := syncIMAP(acct, password)
	if err != nil {
		syncErr := fmt.Errorf("imap sync: %w", err)
		s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", syncErr.Error(), accountID)
		return syncErr
	}

	tx, err := s.db.Begin()
	if err != nil {
		s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", err.Error(), accountID)
		return err
	}
	defer tx.Rollback()

	for _, m := range mails {
		_, err := tx.Exec(`
			INSERT INTO mails (id, account_id, sender, subject, snippet, time, unread)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET unread = excluded.unread`,
			m.ID, accountID, m.Sender, m.Subject, m.Snippet, m.Time, m.Unread,
		)
		if err != nil {
			s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", err.Error(), accountID)
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", err.Error(), accountID)
		return err
	}
	s.db.Exec("UPDATE accounts SET last_sync_error = NULL WHERE id = $1", accountID)
	return nil
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
		if err := s.syncAccount(id); err != nil {
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
