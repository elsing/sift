// Package api serves the mock inbox data for Phase 1. There is no real mail
// source yet — Archive/Delete/Refresh just mutate a Postgres table seeded
// with fake data. Real IMAP/Gmail integration is a later phase.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pageSize = 30

type Mail struct {
	ID             string `json:"id"`
	Sender         string `json:"sender"`
	SenderEmail    string `json:"senderEmail,omitempty"` // the actual address; Sender may be a display name instead
	Subject        string `json:"subject"`
	Snippet        string `json:"snippet"`
	Time           string `json:"time"` // pre-formatted for display (today's time, or a date for older mail)
	Date           string `json:"date"` // RFC3339; the client groups by this (Today/This Week/Last Week/Older)
	Unread         bool   `json:"unread"`
	AccountID      string `json:"accountId,omitempty"` // empty for mock mail; tells the client which account's folders to offer for "move"
	UID            uint32 `json:"uid,omitempty"`       // IMAP UID within Folder; zero for mock mail. Exposed so the client can page a folder view by "older than this UID" — not sensitive, just a mailbox-relative sequence number.
	Folder         string `json:"folder,omitempty"`    // IMAP mailbox this UID belongs to; shown for search results spanning folders
	MessageID      string `json:"-"`                   // RFC 5322 Message-ID — stable across moves/re-syncs, unlike ID; tags are keyed on this
	Tags           []Tag  `json:"tags,omitempty"`
	HasAttachments bool   `json:"hasAttachments,omitempty"` // from BODYSTRUCTURE at sync time — cheap (no body fetch), shown as a paperclip in the list
}

type Tag struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Notify      bool   `json:"notify"`
	InstantMove bool   `json:"instantMove"` // skip the global auto-move delay entirely for this tag
}

var senders = []string{"Alex Chen", "Notion", "Sarah Park", "GitHub", "Mom", "Stripe", "Figma", "Delta", "Sam Lee", "Linear"}
var subjects = []string{
	"Re: Q3 roadmap review", "Your invoice is ready", "Weekend plans?", "New comment on your PR",
	"Flight confirmation", "Payment received", "Design review feedback", "Itinerary change",
	"Lunch tomorrow?", "Issue assigned to you",
}
var snippets = []string{
	"Just wanted to follow up on the thread from yesterday...", "Thanks for the quick turnaround on this, really appreciate it...",
	"Let me know if Saturday works better for you...", "Looks good overall, a couple of small notes...",
	"Your trip details have been updated, please review...", "We have received your payment of...",
	"Could we tighten up the spacing on the header...", "Your gate has changed to B12...",
	"No worries if not, just thought it would be fun...", "Assigned by the triage bot, priority: high...",
}

type Store struct {
	db     *sql.DB
	crypto *encryptor

	broadcaster *broadcaster

	watchCtx     context.Context
	watchMu      sync.Mutex
	watchCancels map[string]context.CancelFunc

	// autoMoveMu serializes autoMoveTaggedMail — it's called opportunistically from
	// several places (every sync, accepting a suggestion, manual tagging, the manual
	// "Move tagged mail now" button), with no locking on which mail rows it's about to
	// move. Two overlapping calls could both select the same candidate; whichever
	// loses the race finds the mail already gone by the time it tries to move it,
	// silently undercounting — which is exactly what made the manual button report
	// "nothing to move" while a concurrent background sync was moving things at the
	// same moment.
	autoMoveMu sync.Mutex

	// selfMovedIntoInboxMu/At guards against notifying about mail we moved into INBOX
	// ourselves (Restore from Trash, or a manual "Move" into the inbox) — IMAP assigns
	// a brand-new UID on a move, which looks identical to genuinely new mail to the
	// UID-watermark check, so without this every restore fired a push as if it were a
	// new arrival.
	selfMovedIntoInboxMu sync.Mutex
	selfMovedIntoInboxAt map[string]time.Time
}

// NewStore assumes db.Migrate has already been run by the caller.
func NewStore(db *sql.DB) (*Store, error) {
	crypto, err := newEncryptor()
	if err != nil {
		return nil, err
	}
	s := &Store{
		db: db, crypto: crypto, broadcaster: newBroadcaster(),
		watchCancels:         make(map[string]context.CancelFunc),
		selfMovedIntoInboxAt: make(map[string]time.Time),
	}

	hasAccounts, err := s.hasAccounts()
	if err != nil {
		return nil, fmt.Errorf("check accounts: %w", err)
	}
	if hasAccounts {
		// a real account exists now; purge any mock rows seeded before it was added
		// (account_id is only ever NULL for mock data, never for real IMAP-synced mail)
		if _, err := db.Exec("DELETE FROM mails WHERE account_id IS NULL"); err != nil {
			return nil, fmt.Errorf("clean up mock mail: %w", err)
		}
		return s, nil
	}

	var count int
	if err := db.QueryRow("SELECT count(*) FROM mails").Scan(&count); err != nil {
		return nil, fmt.Errorf("count mails: %w", err)
	}
	if count == 0 {
		if err := s.reseed(); err != nil {
			return nil, fmt.Errorf("seed mails: %w", err)
		}
	}
	return s, nil
}

func (s *Store) reseed() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM mails"); err != nil {
		return err
	}
	for i := 0; i < 18; i++ {
		m := randomMail()
		sentAt, _ := time.Parse(time.RFC3339, m.Date)
		if _, err := tx.Exec(
			"INSERT INTO mails (id, sender, sender_email, subject, snippet, time, unread, sent_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
			m.ID, m.Sender, m.SenderEmail, m.Subject, m.Snippet, m.Time, m.Unread, sentAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func randomMail() Mail {
	i := rand.Intn(len(senders))
	hour := rand.Intn(12) + 1
	period := "PM"
	if hour < 7 {
		period = "AM"
	}
	sentAt := time.Now().AddDate(0, 0, -rand.Intn(20)) // spread across Today/This Week/Last Week/Older for testing
	handle := strings.ToLower(strings.ReplaceAll(senders[i], " ", "."))
	return Mail{
		ID:          fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(1_000_000)),
		Sender:      senders[i],
		SenderEmail: handle + "@example.com",
		Subject:     subjects[i],
		Snippet:     snippets[i],
		Time:        fmt.Sprintf("%d:%02d %s", hour, rand.Intn(60), period),
		Date:        sentAt.Format(time.RFC3339),
		Unread:      rand.Float64() > 0.4,
	}
}

func (s *Store) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/mails", s.handleList)
	mux.HandleFunc("GET /api/mails/{id}", s.handleGetMail)
	mux.HandleFunc("POST /api/mails/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/mails/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		s.handleRemove(w, r, "archive")
	})
	mux.HandleFunc("POST /api/mails/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		s.handleRemove(w, r, "delete")
	})
	mux.HandleFunc("POST /api/mails/{id}/read", s.handleToggleRead)
	mux.HandleFunc("POST /api/mails/{id}/move", s.handleMove)
	mux.HandleFunc("GET /api/mails/{id}/body", s.handleMailBody)
	mux.HandleFunc("GET /api/mails/{id}/attachments/{index}", s.handleMailAttachment)
	mux.HandleFunc("GET /api/events", s.handleEvents)
}

// accountFilter parses the optional ?accounts=id1,id2 query param. An empty result
// means "all accounts" — the common case, and the only case for a single-account setup.
func accountFilter(r *http.Request) []string {
	raw := r.URL.Query().Get("accounts")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	// Cursor-based, not offset-based: paging by row count breaks the moment a new mail
	// is inserted mid-scroll (the IMAP IDLE watcher can do this at any time) — every row
	// at or after the insertion point shifts by one position, so the next "offset N"
	// either repeats a row already shown or skips one entirely. Paging by "older than
	// the last mail you saw" is immune to that: an insertion elsewhere can't move where
	// an already-seen mail sits relative to its own neighbors.
	before := r.URL.Query().Get("before")
	beforeID := r.URL.Query().Get("beforeId")
	firstPage := beforeID == ""
	accountIDs := accountFilter(r)
	mails, err := s.list(pageSize, accountIDs, before, beforeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// self-heal an empty-but-should-have-mail inbox (e.g. right after a cache-clearing
	// migration) instead of silently staying empty until the next manual pull-to-refresh.
	// Also clear any "backfill exhausted" markers: those were recorded against the old
	// cache, which is gone, so they'd otherwise wrongly block backfill forever even
	// though the freshly-rebuilt cache has plenty more history to pull.
	// Skipped while a filter is active — an empty page there just means "this account
	// has no mail," not "the cache is broken," and resyncing every account regardless
	// of the filter would be pointless work.
	if firstPage && len(mails) == 0 && len(accountIDs) == 0 {
		if _, err := s.db.Exec("UPDATE accounts SET oldest_synced_uid = NULL"); err != nil {
			log.Printf("reset backfill progress: %v", err)
		}
		if err := s.syncAllAccounts(); err != nil {
			log.Printf("auto-sync: %v", err)
		} else if refreshed, err := s.list(pageSize, accountIDs, before, beforeID); err == nil {
			mails = refreshed
		}
	}
	if len(mails) < pageSize {
		// scrolled past what's cached locally; pull another batch of older mail before
		// answering. Backfills every account regardless of the active filter — harmless,
		// and avoids a filter switch immediately looking under-backfilled.
		if _, err := s.backfillAllAccounts(pageSize); err != nil {
			log.Printf("backfill: %v", err)
		} else if refreshed, err := s.list(pageSize, accountIDs, before, beforeID); err == nil {
			mails = refreshed
		}
	}
	writeJSON(w, mails)
}

// handleGetMail returns one mail's metadata by id — used to deep-link a push
// notification straight to the mail it's about, since that mail may not be on the
// currently-loaded page (or may not even be in the inbox view at all).
func (s *Store) handleGetMail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var m Mail
	var sentAt sql.NullTime
	row := s.db.QueryRow(
		"SELECT id, sender, sender_email, subject, snippet, time, unread, coalesce(account_id, ''), sent_at, coalesce(message_id, ''), has_attachments FROM mails WHERE id = $1",
		id,
	)
	if err := row.Scan(&m.ID, &m.Sender, &m.SenderEmail, &m.Subject, &m.Snippet, &m.Time, &m.Unread, &m.AccountID, &sentAt, &m.MessageID, &m.HasAttachments); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sentAt.Valid {
		m.Date = sentAt.Time.Format(time.RFC3339)
	}
	mails := []Mail{m}
	if err := s.attachTags(mails); err == nil {
		m = mails[0]
	}
	writeJSON(w, m)
}

// list returns inbox mail, optionally restricted to accountIDs (empty/nil means all).
// list returns inbox mail, newest first, optionally restricted to accountIDs (empty/nil
// means all) and to mail older than the (before, beforeID) cursor (beforeID == "" means
// the first page). sent_at can be null (e.g. mock mail) — coalesced to -infinity
// consistently in both the cursor comparison and the ORDER BY, so the two stay in sync.
func (s *Store) list(limit int, accountIDs []string, before, beforeID string) ([]Mail, error) {
	query := "SELECT id, sender, sender_email, subject, snippet, time, unread, coalesce(account_id, ''), sent_at, coalesce(message_id, ''), has_attachments FROM mails WHERE folder = 'INBOX'"
	args := []any{}
	if len(accountIDs) > 0 {
		args = append(args, accountIDs)
		query += fmt.Sprintf(" AND account_id = ANY($%d)", len(args))
	}
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
	// id as a tiebreaker just keeps paging deterministic for the rare case of two mails
	// sharing a timestamp.
	args = append(args, limit)
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
		if err := rows.Scan(&m.ID, &m.Sender, &m.SenderEmail, &m.Subject, &m.Snippet, &m.Time, &m.Unread, &m.AccountID, &sentAt, &m.MessageID, &m.HasAttachments); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			m.Date = sentAt.Time.Format(time.RFC3339)
		}
		mails = append(mails, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachTags(mails); err != nil {
		return nil, err
	}
	return mails, nil
}

// attachTags fills in Tags for each mail, batched into one query rather than one
// per row. Mutates mails in place.
func (s *Store) attachTags(mails []Mail) error {
	byMessageID := map[string]*Mail{}
	messageIDs := make([]string, 0, len(mails))
	for i := range mails {
		if mails[i].MessageID == "" {
			continue
		}
		byMessageID[mails[i].MessageID] = &mails[i]
		messageIDs = append(messageIDs, mails[i].MessageID)
	}
	if len(messageIDs) == 0 {
		return nil
	}

	rows, err := s.db.Query(`
		SELECT mt.message_id, t.id, t.name, t.color
		FROM message_tags mt JOIN tags t ON t.id = mt.tag_id
		WHERE mt.message_id = ANY($1) ORDER BY t.name`, messageIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var messageID string
		var tag Tag
		if err := rows.Scan(&messageID, &tag.ID, &tag.Name, &tag.Color); err != nil {
			return err
		}
		if m, ok := byMessageID[messageID]; ok {
			m.Tags = append(m.Tags, tag)
		}
	}
	return rows.Err()
}

func (s *Store) handleRefresh(w http.ResponseWriter, r *http.Request) {
	hasAccounts, err := s.hasAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hasAccounts {
		if err := s.syncAllAccounts(); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	} else if err := s.reseed(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mails, err := s.list(pageSize, accountFilter(r), "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, mails)
}

func (s *Store) handleRemove(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")
	if isDryRun(r) {
		log.Printf("[dry-run] would %s mail %s", action, id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.archiveOrDeleteOnServer(id, action); err != nil {
		log.Printf("%s mail %s on server: %v", action, id, err)
		http.Error(w, fmt.Sprintf("couldn't %s on the server: %v", action, err), http.StatusBadGateway)
		return
	}
	if _, err := s.db.Exec("DELETE FROM mails WHERE id = $1", id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleMove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Folder == "" {
		http.Error(w, "a destination folder is required", http.StatusBadRequest)
		return
	}

	if isDryRun(r) {
		log.Printf("[dry-run] would move mail %s to %s", id, req.Folder)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := s.moveMailToFolder(id, req.Folder); err != nil {
		log.Printf("move mail %s to %s: %v", id, req.Folder, err)
		http.Error(w, "couldn't move that mail: "+err.Error(), http.StatusBadGateway)
		return
	}
	if _, err := s.db.Exec("DELETE FROM mails WHERE id = $1", id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleToggleRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if isDryRun(r) {
		var m Mail
		row := s.db.QueryRow("SELECT id, sender, subject, snippet, time, unread FROM mails WHERE id = $1", id)
		if err := row.Scan(&m.ID, &m.Sender, &m.Subject, &m.Snippet, &m.Time, &m.Unread); err != nil {
			if err == sql.ErrNoRows {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.Unread = !m.Unread
		log.Printf("[dry-run] would toggle read on mail %s", id)
		writeJSON(w, m)
		return
	}

	var m Mail
	row := s.db.QueryRow(
		"UPDATE mails SET unread = NOT unread WHERE id = $1 RETURNING id, sender, subject, snippet, time, unread",
		id,
	)
	if err := row.Scan(&m.ID, &m.Sender, &m.Subject, &m.Snippet, &m.Time, &m.Unread); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

func (s *Store) handleMailBody(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var accountID *string
	var uid *int64
	var folder string
	row := s.db.QueryRow("SELECT account_id, uid, folder FROM mails WHERE id = $1", id)
	if err := row.Scan(&accountID, &uid, &folder); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if accountID == nil || uid == nil {
		writeJSON(w, MailBody{Text: "This is mock mail — there's no real content to show."})
		return
	}

	acct, password, err := s.loadAccountCreds(*accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := fetchMailBody(acct, password, folder, uint32(*uid))
	if err != nil {
		log.Printf("fetch body %s: %v", id, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Remote images (most commonly tracking pixels) load over the network the moment
	// an HTML body renders — the client blocks them by default unless the sender's
	// been explicitly trusted, which this tells it.
	var ownerSubject, senderEmail string
	s.db.QueryRow("SELECT a.owner_subject, coalesce(m.sender_email, '') FROM mails m JOIN accounts a ON a.id = m.account_id WHERE m.id = $1", id).Scan(&ownerSubject, &senderEmail)
	body.SenderEmail = senderEmail
	if ownerSubject != "" && senderEmail != "" {
		s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM trusted_senders WHERE owner_subject = $1 AND sender_email = $2)", ownerSubject, senderEmail).Scan(&body.TrustedSender)
	}
	writeJSON(w, body)
}

func (s *Store) handleMailAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		http.Error(w, "bad attachment index", http.StatusBadRequest)
		return
	}
	var accountID *string
	var uid *int64
	var folder string
	if err := s.db.QueryRow("SELECT account_id, uid, folder FROM mails WHERE id = $1", id).Scan(&accountID, &uid, &folder); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if accountID == nil || uid == nil {
		http.NotFound(w, r)
		return
	}
	acct, password, err := s.loadAccountCreds(*accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	att, raw, err := fetchMailAttachment(acct, password, folder, uint32(*uid), index)
	if err != nil {
		log.Printf("fetch attachment %s/%d: %v", id, index, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if att.ContentType != "" {
		w.Header().Set("Content-Type", att.ContentType)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", att.Filename))
	w.Write(raw)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
