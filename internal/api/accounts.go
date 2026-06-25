package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type dbAccount struct {
	ID            string
	Email         string
	Host          string
	Port          int
	Username      string
	OAuthProvider string // "" for password accounts, "google" for OAuth-linked ones
}

type accountOut struct {
	ID              string   `json:"id"`
	Email           string   `json:"email"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	LastSyncError   string   `json:"lastSyncError,omitempty"`
	ExpandedFolders []string `json:"expandedFolders"`
	TrashFolder     string   `json:"trashFolder,omitempty"` // best-effort, cached only — lets the client offer "Restore" when browsing Trash
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
	mux.HandleFunc("POST /api/accounts/{id}/folders", s.handleCreateFolder)
	mux.HandleFunc("PATCH /api/accounts/{id}/folders", s.handleRenameFolder)
	mux.HandleFunc("DELETE /api/accounts/{id}/folders", s.handleDeleteFolder)
	mux.HandleFunc("GET /api/accounts/{id}/folder-mails", s.handleFolderMails)
	mux.HandleFunc("PUT /api/accounts/{id}/expanded-folders", s.handleSetExpandedFolders)
	mux.HandleFunc("GET /api/folders/cleanup-duplicates", func(w http.ResponseWriter, r *http.Request) {
		s.handleCleanupDuplicateFolderCache(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/folders/cleanup-duplicates/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.handleCancelOwnerJob(w, r, ownerSubject(r), "cleanup-duplicate-folders")
	})
}

// isDryRun reports whether the request asked to simulate a mutation rather than perform it.
func isDryRun(r *http.Request) bool {
	return r.Header.Get("X-Dry-Run") == "1"
}

// loadAccountCreds fetches and decrypts the IMAP credentials for an account. For an
// OAuth-linked account the returned "password" is actually a fresh access token —
// refreshed here if the cached one has expired — and acct.OAuthProvider tells every
// IMAP call site (via dialIMAP/imapAuth) to authenticate with OAUTHBEARER instead of
// plain LOGIN.
func (s *Store) loadAccountCreds(id string) (dbAccount, string, error) {
	var acct dbAccount
	var encPassword, encRefresh, encAccess []byte
	var tokenExpiry *time.Time
	err := s.db.QueryRow(
		`SELECT id, email, host, port, username, password_enc, oauth_provider, oauth_refresh_token_enc, oauth_access_token_enc, oauth_token_expiry
		 FROM accounts WHERE id = $1`, id,
	).Scan(&acct.ID, &acct.Email, &acct.Host, &acct.Port, &acct.Username, &encPassword,
		&acct.OAuthProvider, &encRefresh, &encAccess, &tokenExpiry)
	if err != nil {
		return acct, "", fmt.Errorf("load account: %w", err)
	}

	if acct.OAuthProvider == "" {
		password, err := s.crypto.decrypt(encPassword)
		if err != nil {
			return acct, "", fmt.Errorf("decrypt password: %w", err)
		}
		return acct, password, nil
	}

	// Refresh a little early (60s) rather than right at expiry, so a slow IMAP call
	// started just before expiry doesn't get cut off mid-request by Gmail.
	if tokenExpiry != nil && time.Now().Add(60*time.Second).Before(*tokenExpiry) {
		access, err := s.crypto.decrypt(encAccess)
		if err == nil {
			return acct, access, nil
		}
	}
	refresh, err := s.crypto.decrypt(encRefresh)
	if err != nil {
		return acct, "", fmt.Errorf("decrypt refresh token: %w", err)
	}
	tok, err := refreshGoogleAccessToken(context.Background(), refresh)
	if err != nil {
		return acct, "", fmt.Errorf("refresh google token: %w", err)
	}
	encNewAccess, err := s.crypto.encrypt(tok.AccessToken)
	if err == nil {
		s.db.Exec("UPDATE accounts SET oauth_access_token_enc = $1, oauth_token_expiry = $2 WHERE id = $3", encNewAccess, tok.Expiry, id)
	}
	return acct, tok.AccessToken, nil
}

func (s *Store) handleListAccounts(w http.ResponseWriter, r *http.Request, owner string) {
	rows, err := s.db.Query(
		"SELECT id, email, host, port, coalesce(last_sync_error, ''), expanded_folders, coalesce(trash_folder, '') FROM accounts WHERE owner_subject = $1 ORDER BY created_at",
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
		if err := rows.Scan(&a.ID, &a.Email, &a.Host, &a.Port, &a.LastSyncError, &expandedJSON, &a.TrashFolder); err != nil {
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
	// Best-effort, right away rather than waiting for whatever else happens to need it
	// first (an archive/delete) — without this, "Restore" in Trash wouldn't show up
	// until something else had already triggered this same lookup.
	if _, trash, _, err := s.resolveFolders(id); err == nil {
		out.TrashFolder = trash
	}
	s.cleanupMockMail()
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

// cachedListFolders serves the server-side folder_cache if present (any age — this
// app's own create/rename/delete handlers refresh it directly after every mutation
// they make, so there's no external drift to guard against with a TTL the way the
// image proxy's cache needs one). Falls back to a live IMAP LIST on a genuine miss
// (a brand-new account, or the cache row never having been written yet).
func (s *Store) cachedListFolders(acct dbAccount, password string) ([]string, rune, error) {
	var names []string
	var delim string
	err := s.db.QueryRow("SELECT names, delim FROM folder_cache WHERE account_id = $1", acct.ID).Scan(&names, &delim)
	if err == nil {
		var d rune
		if delim != "" {
			d = rune(delim[0])
		}
		return names, d, nil
	}
	return s.refreshFolderCache(acct, password)
}

// refreshFolderCache does the actual live IMAP LIST and writes the result back —
// called both on a cache miss (cachedListFolders above) and proactively after this
// app's own folder mutations, so the next read is instant instead of starting cold.
func (s *Store) refreshFolderCache(acct dbAccount, password string) ([]string, rune, error) {
	names, delim, err := listFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
	if err != nil {
		return nil, 0, err
	}
	s.db.Exec(
		`INSERT INTO folder_cache (account_id, names, delim) VALUES ($1, $2, $3)
		 ON CONFLICT (account_id) DO UPDATE SET names = excluded.names, delim = excluded.delim, fetched_at = now()`,
		acct.ID, names, string(delim),
	)
	return names, delim, nil
}

// watchFolderChanges periodically re-checks every account's folder list directly over
// IMAP and publishes "folders" if anything changed — this app's own create/rename/
// delete already refresh+publish immediately and don't need to wait for this, but a
// folder added/renamed/removed through some *other* mail client (the actual webmail,
// Outlook, whatever) has no other way to ever reach this app's cache otherwise. Every
// 2 hours, not on every sync — an IMAP LIST per account is cheap enough, but there's
// no reason to run it any more often than something might plausibly have changed.
// FOLDER_CHECK_INTERVAL_HOURS overrides how often (default 2h); mainly useful for
// testing this without waiting two real hours for a tick.
func folderCheckInterval() time.Duration {
	if v := os.Getenv("FOLDER_CHECK_INTERVAL_HOURS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			return time.Duration(n * float64(time.Hour))
		}
	}
	return 2 * time.Hour
}

func (s *Store) watchFolderChanges(ctx context.Context) {
	ticker := time.NewTicker(folderCheckInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAllAccountsForFolderChanges()
		}
	}
}

// folderMailSyncLimit caps how many of each folder's most recent mails the periodic
// sync keeps fresh — mirrors scanFolderLimit (smarttags.go); deep history in a rarely-
// opened folder isn't worth an IMAP round trip every cycle, the first live page load
// (or pull-to-refresh) into it backfills further if you actually scroll that deep.
const folderMailSyncLimit = 200

// watchFolderMailSync is what actually makes handleFolderMails' "no IMAP round trip
// for ordinary browsing" claim true: every folderCheckInterval, every account's every
// folder gets its recent mail re-fetched and upserted into the DB directly — not
// triggered by, or waited on by, any client page load. A folder's own IMAP IDLE watch
// (syncAccount) only ever covers INBOX; this is what keeps every other folder's local
// copy from just going stale forever between visits.
func (s *Store) watchFolderMailSync(ctx context.Context) {
	s.syncAllFolderMail(ctx)
	ticker := time.NewTicker(folderCheckInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncAllFolderMail(ctx)
		}
	}
}

func (s *Store) syncAllFolderMail(ctx context.Context) {
	rows, err := s.db.Query("SELECT id FROM accounts")
	if err != nil {
		log.Printf("folder mail sync: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		acct, password, err := s.loadAccountCreds(id)
		if err != nil {
			continue
		}
		names, _, err := listFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
		if err != nil {
			log.Printf("folder mail sync %s: list folders: %v", acct.Email, err)
			continue
		}
		// walkAccountFolders upserts every mail it finds into the DB itself — nothing
		// extra to do per mail here, this loop just needs to run. Stops after the first
		// page (onMail returns false) — this runs every folderCheckInterval forever, so
		// re-walking a folder's entire history on every tick just to keep "recent" fresh
		// would be enormously wasteful; full-history callers (the scans, the image
		// backfill) opt into that explicitly by returning true instead.
		if err := s.walkAccountFolders(ctx, acct, password, id, names, folderMailSyncLimit, false, nil, func(*imapclient.Client, Mail, MailBody, string) bool { return false }, nil); err != nil {
			log.Printf("folder mail sync %s: %v", acct.Email, err)
		}
	}
}

func (s *Store) checkAllAccountsForFolderChanges() {
	rows, err := s.db.Query("SELECT id FROM accounts")
	if err != nil {
		log.Printf("check folder changes: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	for _, id := range ids {
		acct, password, err := s.loadAccountCreds(id)
		if err != nil {
			continue
		}
		var before []string
		s.db.QueryRow("SELECT names FROM folder_cache WHERE account_id = $1", id).Scan(&before)
		after, _, err := s.refreshFolderCache(acct, password)
		if err != nil {
			log.Printf("check folder changes %s: %v", acct.Email, err)
			continue
		}
		if !slices.Equal(before, after) {
			s.broadcaster.publish("folders")
		}
	}
}

func (s *Store) handleListFolders(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// ?force=1 skips the server cache too, not just the client's — used when opening
	// Settings > Manage folders, the one place you're actively editing folders and
	// want the genuine current state rather than whatever's cached, even briefly.
	var names []string
	var delim rune
	if r.URL.Query().Get("force") == "1" {
		names, delim, err = s.refreshFolderCache(acct, password)
	} else {
		names, delim, err = s.cachedListFolders(acct, password)
	}
	if err != nil {
		log.Printf("list folders %s: %v", acct.Email, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, buildFolderTree(names, delim))
}

func (s *Store) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Parent + name, not a pre-joined path — the client has no reliable way to know
	// this account's hierarchy delimiter (it only ever sees paths the server already
	// joined), so joining happens here instead of guessing "/" client-side.
	var req struct{ ParentPath, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_, delim, err := s.cachedListFolders(acct, password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	sep := string(delim)
	if sep == "" {
		sep = "/"
	}
	path := req.Name
	if req.ParentPath != "" {
		path = req.ParentPath + sep + req.Name
	}
	if err := createFolder(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, path); err != nil {
		log.Printf("create folder %s/%s: %v", acct.Email, path, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Refreshed right away rather than just invalidated — the next read (almost
	// certainly this same request's response landing in the UI) is then instant
	// instead of starting cold on whatever happens to ask first. Published over SSE so
	// any client sitting on a much longer-lived local cache (24h) knows to drop it too,
	// rather than only this one request's own view staying correct.
	if _, _, err := s.refreshFolderCache(acct, password); err != nil {
		log.Printf("refresh folder cache %s: %v", acct.Email, err)
	}
	s.broadcaster.publish("folders")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleRenameFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Path + new leaf name, not two full paths — renaming only ever changes this
	// folder's own name in place here (moving to a different parent isn't exposed),
	// and computing the new full path needs the account's actual delimiter rather
	// than the client guessing one.
	var req struct{ Path, NewName string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || req.NewName == "" {
		http.Error(w, "path and newName are required", http.StatusBadRequest)
		return
	}
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_, delim, err := s.cachedListFolders(acct, password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	sep := string(delim)
	if sep == "" {
		sep = "/"
	}
	to := req.NewName
	if i := strings.LastIndex(req.Path, sep); i >= 0 {
		to = req.Path[:i] + sep + req.NewName
	}
	if err := renameFolder(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, req.Path, to); err != nil {
		log.Printf("rename folder %s/%s->%s: %v", acct.Email, req.Path, to, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, _, err := s.refreshFolderCache(acct, password); err != nil {
		log.Printf("refresh folder cache %s: %v", acct.Email, err)
	}
	s.broadcaster.publish("folders")
	// Best-effort: a folder this app already knew about (a tag-destination rule, mail
	// cached under its old path) shouldn't silently keep pointing at a name that no
	// longer exists just because the rename itself succeeded.
	s.db.Exec("UPDATE folder_tag_rules SET folder = $1 WHERE account_id = $2 AND folder = $3", to, id, req.Path)
	s.db.Exec("UPDATE mails SET folder = $1 WHERE account_id = $2 AND folder = $3", to, id, req.Path)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct{ Path string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := deleteFolder(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, req.Path); err != nil {
		log.Printf("delete folder %s/%s: %v", acct.Email, req.Path, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, _, err := s.refreshFolderCache(acct, password); err != nil {
		log.Printf("refresh folder cache %s: %v", acct.Email, err)
	}
	s.broadcaster.publish("folders")
	s.db.Exec("DELETE FROM folder_tag_rules WHERE account_id = $1 AND folder = $2", id, req.Path)
	s.db.Exec("DELETE FROM mails WHERE account_id = $1 AND folder = $2", id, req.Path)
	w.WriteHeader(http.StatusNoContent)
}

// cachedFolderMails reads whatever's already in the local mails table for this
// account+folder — populated by a previous live fetch, the periodic full-folder sync
// (watchFolderMailSync), or any of the scan features that happen to walk this folder.
// No IMAP round trip, so it's instant. before/beforeID is the same cursor-tuple
// pagination handleList (mails.go) uses for the inbox — immune to a new mail landing
// mid-scroll shifting every row's offset, unlike paging by row count.
func (s *Store) cachedFolderMails(accountID, folder, before, beforeID string) ([]Mail, error) {
	query := `SELECT id, sender, sender_email, subject, snippet, time, unread, sent_at, coalesce(message_id, ''), has_attachments
		 FROM mails WHERE account_id = $1 AND folder = $2`
	args := []any{accountID, folder}
	if beforeID != "" {
		var beforeArg any
		if before != "" {
			beforeArg = before
		}
		args = append(args, beforeArg, beforeID)
		query += fmt.Sprintf(
			" AND (coalesce(sent_at, '-infinity'::timestamptz), id) < (coalesce($%d::timestamptz, '-infinity'::timestamptz), $%d)",
			len(args)-1, len(args),
		)
	}
	args = append(args, pageSize)
	query += fmt.Sprintf(" ORDER BY coalesce(sent_at, '-infinity'::timestamptz) DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mails := []Mail{}
	for rows.Next() {
		var m Mail
		var sentAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.Sender, &m.SenderEmail, &m.Subject, &m.Snippet, &m.Time, &m.Unread, &sentAt, &m.MessageID, &m.HasAttachments); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			m.Date = sentAt.Time.Format(time.RFC3339)
		}
		m.AccountID = accountID
		m.Folder = folder
		mails = append(mails, m)
	}
	return mails, rows.Err()
}

func (s *Store) handleFolderMails(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	folder := r.URL.Query().Get("path")
	if folder == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	// DB-backed by default — no IMAP round trip for ordinary folder browsing. The DB
	// copy is kept fresh by the periodic full sync (watchFolderMailSync, every
	// folderCheckInterval) and by the IMAP IDLE push for whichever folder that's
	// watching, not by each page load doing its own live fetch. live=1 is the explicit
	// override (pull-to-refresh while browsing a folder) that actually goes to IMAP.
	if r.URL.Query().Get("live") != "1" {
		before := r.URL.Query().Get("before")
		beforeID := r.URL.Query().Get("beforeId")
		mails, err := s.cachedFolderMails(id, folder, before, beforeID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.attachTags(mails)
		writeJSON(w, mails)
		return
	}
	acct, password, err := s.loadAccountCreds(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	beforeUID, _ := strconv.ParseUint(r.URL.Query().Get("beforeUid"), 10, 32)
	mails, err := fetchFolderMail(acct, password, folder, pageSize, uint32(beforeUID))
	if err != nil {
		log.Printf("folder mails %s/%s: %v", acct.Email, folder, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// mailsFromFetch never sets AccountID on the Mail structs it builds (every other
	// caller tracks it separately and feeds it to upsertMails as its own parameter) —
	// but the client needs it on the mail object itself to know which account's
	// folders to offer for Move. Without this, currentMail.accountId was always empty
	// for anything opened from a folder view, and the reader's Move button silently
	// did nothing.
	for i := range mails {
		mails[i].AccountID = id
	}
	// persist so archive/delete/move/read endpoints (which look mail up by id) work
	// the same inside a folder view as they do in the inbox.
	if err := s.upsertMails(id, mails); err != nil {
		log.Printf("cache folder mails %s/%s: %v", acct.Email, folder, err)
	}
	// the inbox list path (list()) does this too — folder mail just never picked it up
	// when this endpoint was first written, so tag chips silently never showed here.
	if err := s.attachTags(mails); err != nil {
		log.Printf("attach tags %s/%s: %v", acct.Email, folder, err)
	}
	if mails == nil {
		mails = []Mail{} // an empty folder's nil slice would otherwise encode as JSON null, not []
	}
	writeJSON(w, mails)
}

func (s *Store) handleSyncAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.syncAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	mails, err := s.list(pageSize, nil, "", "")
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

	// Smart-tagging needs to know which of these are genuinely new, not just a resync
	// of mail already seen — upsertMails' ON CONFLICT DO UPDATE can't tell new from
	// re-synced on its own. UIDs only increase within a mailbox (the same invariant
	// notifyNewMail already relies on), so anything above the watermark from last time
	// is new. Read it before upserting, evaluate after, so a slow scorer never delays
	// the mail actually showing up.
	var lastSeenUID *int64
	s.db.QueryRow("SELECT last_seen_uid FROM accounts WHERE id = $1", accountID).Scan(&lastSeenUID)
	var newMails []Mail
	var maxUID uint32
	for _, m := range mails {
		if lastSeenUID == nil || int64(m.UID) > *lastSeenUID {
			newMails = append(newMails, m)
		}
		if m.UID > maxUID {
			maxUID = m.UID
		}
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
	if maxUID > 0 {
		s.db.Exec(
			"UPDATE accounts SET last_seen_uid = GREATEST(coalesce(last_seen_uid, 0), $1) WHERE id = $2",
			int64(maxUID), accountID,
		)
	}
	// On an account's first-ever sync lastSeenUID is nil, so nothing counts as "new" —
	// deliberately avoids flooding suggestions against the whole pre-existing inbox the
	// moment an account is added.
	if lastSeenUID != nil && len(newMails) > 0 {
		s.evaluateNewMailForSmartTags(accountID, newMails)
		// Was notifying off the full inbox re-fetch instead of just newMails — any IDLE
		// wake re-syncs and notified about "highest UID in the mailbox" regardless of
		// why the wake happened. Moving mail OUT of inbox (e.g. auto-move) triggers the
		// same IDLE "mailbox changed" signal as new mail arriving, which meant every
		// auto-moved mail was good for a spurious push about old, already-seen mail
		// that happened to still be sitting at the top of the inbox.
		s.notifyNewMail(accountID, newMails)
		// Fire-and-forget: warms the image proxy cache (imageproxy.go) ahead of you ever
		// opening these mails, so a sender can't infer open-time from proxy fetch timing
		// even with the IP itself already hidden. Each mail needs its own body fetch
		// (sync only ever pulled envelope/flags), so this runs off the sync's own
		// goroutine rather than block the response on it.
		go s.prefetchMailImages(acct, password, newMails)
	}
	s.maybeCleanupImageCache()
	// Not gated on new mail — this is about time elapsed since tagging, not what just
	// arrived, so it should run every sync regardless.
	var ownerSubject string
	if s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject) == nil {
		s.autoMoveTaggedMail(ownerSubject)
	}
	return mails, nil
}

// handleCleanupDuplicateFolderCache is the one-off catch-up for a structural gap:
// mails.id encodes account|folder|uid, so a message moved between folders by anything
// other than Sift's own move action (the user's own mail client, webmail, a
// server-side rule) leaves a stale "ghost" row behind under its old folder — the
// upsert recording its new location never touches the old id. upsertMails now cleans
// this up going forward for anything it re-fetches; this is the retroactive fix for
// ghosts that already exist. There's no "last synced" timestamp on mails to tell which
// of two candidate rows is current, so each affected message gets checked live over
// IMAP — the only reliable source of truth — and whichever cached copy is confirmed
// gone from its folder gets removed. A copy that can't be confirmed either way (a
// lookup failure) is left alone rather than risking deleting a real one.
// Job-backed (handleOwnerJobSSE, scanjobs.go) — a real backlog here means hundreds of
// live IMAP lookups, easily slow enough to need persistence and progress, not a single
// connection that dies the moment a tab closes or a deploy restarts the server.
func (s *Store) handleCleanupDuplicateFolderCache(w http.ResponseWriter, r *http.Request, owner string) {
	s.handleOwnerJobSSE(w, r, owner, "cleanup-duplicate-folders", func(ctx context.Context, onProgress func(done, total int)) (any, error) {
		return s.cleanupDuplicateFolderCache(ctx, owner, onProgress)
	})
}

func (s *Store) cleanupDuplicateFolderCache(ctx context.Context, owner string, onProgress func(done, total int)) (any, error) {
	rows, err := s.db.Query(`
		SELECT m.message_id, m.account_id
		FROM mails m
		JOIN accounts a ON a.id = m.account_id
		WHERE a.owner_subject = $1 AND m.message_id IS NOT NULL AND m.message_id != ''
		GROUP BY m.message_id, m.account_id
		HAVING count(DISTINCT m.folder) > 1`,
		owner,
	)
	if err != nil {
		return nil, err
	}
	type candidate struct{ messageID, accountID string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.messageID, &c.accountID) == nil {
			candidates = append(candidates, c)
		}
	}
	rows.Close()

	type acctCreds struct {
		acct     dbAccount
		password string
	}
	credsCache := map[string]acctCreds{}

	removed := 0
	for i, c := range candidates {
		if ctx.Err() != nil {
			break // job cancelled, or the server's shutting down — stop burning IMAP round trips
		}
		creds, ok := credsCache[c.accountID]
		if !ok {
			acct, password, err := s.loadAccountCreds(c.accountID)
			if err != nil {
				if onProgress != nil {
					onProgress(i+1, len(candidates))
				}
				continue
			}
			creds = acctCreds{acct, password}
			credsCache[c.accountID] = creds
		}

		folderRows, err := s.db.Query("SELECT id, folder FROM mails WHERE account_id = $1 AND message_id = $2", c.accountID, c.messageID)
		if err != nil {
			if onProgress != nil {
				onProgress(i+1, len(candidates))
			}
			continue
		}
		type cached struct{ id, folder string }
		var cachedRows []cached
		for folderRows.Next() {
			var cr cached
			if folderRows.Scan(&cr.id, &cr.folder) == nil {
				cachedRows = append(cachedRows, cr)
			}
		}
		folderRows.Close()

		for _, cr := range cachedRows {
			uid, err := findUIDByMessageID(creds.acct.Host, creds.acct.Port, creds.acct.Username, creds.password, creds.acct.OAuthProvider, cr.folder, c.messageID)
			if err != nil {
				continue
			}
			if uid == 0 {
				if _, err := s.db.Exec("DELETE FROM mails WHERE id = $1", cr.id); err == nil {
					removed++
				}
			}
		}
		if onProgress != nil {
			onProgress(i+1, len(candidates))
		}
	}
	return map[string]int{"messagesChecked": len(candidates), "ghostRowsRemoved": removed}, nil
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
			INSERT INTO mails (id, account_id, sender, sender_email, subject, snippet, time, unread, uid, folder, sent_at, message_id, has_attachments)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (id) DO UPDATE SET
				sender = excluded.sender, sender_email = excluded.sender_email, subject = excluded.subject,
				snippet = excluded.snippet, time = excluded.time, unread = excluded.unread,
				uid = excluded.uid, folder = excluded.folder, sent_at = excluded.sent_at, message_id = excluded.message_id,
				has_attachments = excluded.has_attachments`,
			m.ID, accountID, m.Sender, m.SenderEmail, m.Subject, m.Snippet, m.Time, m.Unread, m.UID, m.Folder, sentAt, m.MessageID, m.HasAttachments,
		)
		if err != nil {
			return err
		}
		// id encodes account|folder|uid, so a message moved by anything other than
		// Sift's own move action (the user's own mail client, webmail, a server-side
		// rule) lands under a brand new id here — the ON CONFLICT above never touches
		// the OLD row, which just sits there forever under its old folder, looking
		// like the same mail exists in two places at once (it's really one current
		// copy plus a stale ghost of where it used to be). Anything else cached for
		// this account under the same Message-ID but a different id is exactly that
		// ghost, now that this fetch has confirmed where the message actually is.
		if m.MessageID != "" {
			if _, err := tx.Exec(
				"DELETE FROM mails WHERE account_id = $1 AND message_id = $2 AND id != $3",
				accountID, m.MessageID, m.ID,
			); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// resolveFolders returns the account's Archive/Trash mailbox names, detecting and
// caching them on first use so we don't re-run a full folder LIST on every swipe.
// Either value can be "" if the server has no such folder.
func (s *Store) resolveFolders(accountID string) (archive, trash, junk string, err error) {
	var archiveCol, trashCol, junkCol *string
	err = s.db.QueryRow(
		"SELECT archive_folder, trash_folder, junk_folder FROM accounts WHERE id = $1", accountID,
	).Scan(&archiveCol, &trashCol, &junkCol)
	if err != nil {
		return "", "", "", err
	}
	if archiveCol != nil && trashCol != nil && junkCol != nil {
		return *archiveCol, *trashCol, *junkCol, nil
	}

	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return "", "", "", err
	}
	archive, trash, junk, err = detectSpecialUseFolders(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider)
	if err != nil {
		return "", "", "", err
	}
	s.db.Exec("UPDATE accounts SET archive_folder = $1, trash_folder = $2, junk_folder = $3 WHERE id = $4", archive, trash, junk, accountID)
	return archive, trash, junk, nil
}

// moveMailToFolder loads a mail's account + UID + source folder and moves it to
// destFolder on the real IMAP server.
func (s *Store) moveMailToFolder(mailID, destFolder string) error {
	var accountID *string
	var uid *int64
	var sourceFolder, messageID string
	err := s.db.QueryRow(
		"SELECT account_id, uid, folder, coalesce(message_id, '') FROM mails WHERE id = $1", mailID,
	).Scan(&accountID, &uid, &sourceFolder, &messageID)
	if err != nil {
		return err
	}
	if accountID == nil || uid == nil {
		return fmt.Errorf("mock mail has no real mailbox to move it in")
	}
	if sourceFolder == destFolder {
		// IMAP has no real notion of "move to the same mailbox" — issuing one anyway
		// copies the message to a new UID in the same folder and expunges the old one,
		// which looks like a no-op but actually duplicates the message every single
		// time it runs. A bogus folder_tag_rule mapping a tag back to its own source
		// folder (e.g. an inbox-scoped tag rule) made this fire repeatedly via the
		// opportunistic auto-move path, producing several real, separate copies of the
		// same mail on the server. Guard here so nothing can do this again, regardless
		// of which caller or which bad rule tries.
		return nil
	}
	acct, password, err := s.loadAccountCreds(*accountID)
	if err != nil {
		return err
	}
	if err := moveMail(acct.Host, acct.Port, acct.Username, password, acct.OAuthProvider, uint32(*uid), sourceFolder, destFolder); err != nil {
		return err
	}
	if destFolder == "INBOX" {
		s.markSelfMovedIntoInbox(*accountID)
	}
	s.applyFolderTagRule(*accountID, destFolder, messageID)
	return nil
}

// markSelfMovedIntoInbox/recentlySelfMovedIntoInbox — see selfMovedIntoInboxAt's own
// comment on Store. The window just needs to comfortably outlast "the IDLE wake this
// move itself triggers, plus the sync it causes" — a few seconds, not a real session.
func (s *Store) markSelfMovedIntoInbox(accountID string) {
	s.selfMovedIntoInboxMu.Lock()
	defer s.selfMovedIntoInboxMu.Unlock()
	s.selfMovedIntoInboxAt[accountID] = time.Now()
}

func (s *Store) recentlySelfMovedIntoInbox(accountID string) bool {
	s.selfMovedIntoInboxMu.Lock()
	defer s.selfMovedIntoInboxMu.Unlock()
	t, ok := s.selfMovedIntoInboxAt[accountID]
	return ok && time.Since(t) < 30*time.Second
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

	archive, trash, _, err := s.resolveFolders(*accountID)
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
