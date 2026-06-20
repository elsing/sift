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
	"time"
)

const pageSize = 30

type Mail struct {
	ID        string `json:"id"`
	Sender    string `json:"sender"`
	Subject   string `json:"subject"`
	Snippet   string `json:"snippet"`
	Time      string `json:"time"`
	Unread    bool   `json:"unread"`
	AccountID string `json:"accountId,omitempty"` // empty for mock mail; tells the client which account's folders to offer for "move"
	UID       uint32 `json:"-"`                   // IMAP UID within INBOX; zero for mock mail, used to move/delete on the server
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
		if _, err := tx.Exec(
			"INSERT INTO mails (id, sender, subject, snippet, time, unread) VALUES ($1, $2, $3, $4, $5, $6)",
			m.ID, m.Sender, m.Subject, m.Snippet, m.Time, m.Unread,
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
	return Mail{
		ID:      fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(1_000_000)),
		Sender:  senders[i],
		Subject: subjects[i],
		Snippet: snippets[i],
		Time:    fmt.Sprintf("%d:%02d %s", hour, rand.Intn(60), period),
		Unread:  rand.Float64() > 0.4,
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
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	mails, err := s.list(offset, pageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
		"SELECT id, sender, subject, snippet, time, unread, coalesce(account_id, '') FROM mails ORDER BY id DESC LIMIT $1 OFFSET $2",
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mails := []Mail{}
	for rows.Next() {
		var m Mail
		if err := rows.Scan(&m.ID, &m.Sender, &m.Subject, &m.Snippet, &m.Time, &m.Unread, &m.AccountID); err != nil {
			return nil, err
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
