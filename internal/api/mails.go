// Package api serves the mock inbox data for Phase 1. There is no real mail
// source yet — Archive/Delete/Refresh just mutate a Postgres table seeded
// with fake data. Real IMAP/Gmail integration is a later phase.
package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const pageSize = 30

type Mail struct {
	ID          string `json:"id"`
	Sender      string `json:"sender"`
	SenderEmail string `json:"senderEmail,omitempty"` // the actual address; Sender may be a display name instead
	Subject     string `json:"subject"`
	Snippet     string `json:"snippet"`
	Time        string `json:"time"` // pre-formatted for display (today's time, or a date for older mail)
	Date        string `json:"date"` // RFC3339; the client groups by this (Today/This Week/Last Week/Older)
	Unread      bool   `json:"unread"`
	AccountID   string `json:"accountId,omitempty"` // empty for mock mail; tells the client which account's folders to offer for "move"
	UID         uint32 `json:"-"`                   // IMAP UID within Folder; zero for mock mail, used to move/delete/read on the server
	Folder      string `json:"-"`                   // IMAP mailbox this UID belongs to; "INBOX" for mock mail
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
}

// NewStore assumes db.Migrate has already been run by the caller.
func NewStore(db *sql.DB) (*Store, error) {
	crypto, err := newEncryptor()
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, crypto: crypto}

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
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	mails, err := s.list(offset, pageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// self-heal an empty-but-should-have-mail inbox (e.g. right after a cache-clearing
	// migration) instead of silently staying empty until the next manual pull-to-refresh.
	// Also clear any "backfill exhausted" markers: those were recorded against the old
	// cache, which is gone, so they'd otherwise wrongly block backfill forever even
	// though the freshly-rebuilt cache has plenty more history to pull.
	if offset == 0 && len(mails) == 0 {
		if _, err := s.db.Exec("UPDATE accounts SET oldest_synced_uid = NULL"); err != nil {
			log.Printf("reset backfill progress: %v", err)
		}
		if err := s.syncAllAccounts(); err != nil {
			log.Printf("auto-sync: %v", err)
		} else if refreshed, err := s.list(offset, pageSize); err == nil {
			mails = refreshed
		}
	}
	if len(mails) < pageSize {
		// scrolled past what's cached locally; pull another batch of older mail before answering
		if _, err := s.backfillAllAccounts(pageSize); err != nil {
			log.Printf("backfill: %v", err)
		} else if refreshed, err := s.list(offset, pageSize); err == nil {
			mails = refreshed
		}
	}
	writeJSON(w, mails)
}

func (s *Store) list(offset, limit int) ([]Mail, error) {
	rows, err := s.db.Query(
		"SELECT id, sender, sender_email, subject, snippet, time, unread, coalesce(account_id, ''), sent_at FROM mails WHERE folder = 'INBOX' ORDER BY id DESC LIMIT $1 OFFSET $2",
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mails := []Mail{}
	for rows.Next() {
		var m Mail
		var sentAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.Sender, &m.SenderEmail, &m.Subject, &m.Snippet, &m.Time, &m.Unread, &m.AccountID, &sentAt); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			m.Date = sentAt.Time.Format(time.RFC3339)
		}
		mails = append(mails, m)
	}
	return mails, rows.Err()
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
	mails, err := s.list(0, pageSize)
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
	writeJSON(w, body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
