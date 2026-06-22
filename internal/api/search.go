package api

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const searchResultLimit = 50

// "light" mode searches headers only (Subject/From) — cheap for the server, used for
// search-as-you-type. "deep" searches the full message TEXT (headers + body) — far
// more thorough but far slower across a many-folder mailbox, so it's an explicit
// action rather than something that fires on every keystroke.
const (
	lightSearchTimeout = 15 * time.Second
	deepSearchTimeout  = 45 * time.Second
)

// maxConcurrentFolderSearches bounds how many folders, across all accounts combined,
// are searched at once. A single IMAP connection is stateful (SELECT changes which
// mailbox it's on), so real per-folder concurrency needs one connection per concurrent
// folder — capped here so a many-folder mailbox doesn't open dozens of simultaneous
// connections and trip a provider's connection limit.
const maxConcurrentFolderSearches = 8

// SearchRoutes registers the search endpoint. Separate from Routes/AccountsRoutes
// since it needs the owner subject (to scope which accounts can be searched) the same
// way AccountsRoutes does.
func (s *Store) SearchRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		s.handleSearch(w, r, ownerSubject(r))
	})
}

// searchByTagsOnly returns cached mail across the given accounts carrying any of the
// given tags — no IMAP search, just the local cache, since that's the only place
// tag data exists. Results are limited to whatever's currently cached: a tagged mail
// evicted from the cache (archived/moved long ago) won't show up here.
func (s *Store) searchByTagsOnly(accountIDs, tagIDs []string) ([]Mail, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT m.id, m.sender, m.sender_email, m.subject, m.snippet, m.time, m.unread,
		       coalesce(m.account_id, ''), m.sent_at, coalesce(m.message_id, ''), coalesce(m.folder, ''), coalesce(m.uid, 0), m.has_attachments
		FROM mails m
		JOIN message_tags mt ON mt.message_id = m.message_id
		WHERE m.account_id = ANY($1) AND mt.tag_id = ANY($2) AND m.message_id != ''
		ORDER BY coalesce(m.sent_at, '-infinity'::timestamptz) DESC
		LIMIT $3`,
		accountIDs, tagIDs, searchResultLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mails := []Mail{}
	for rows.Next() {
		var m Mail
		var sentAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.Sender, &m.SenderEmail, &m.Subject, &m.Snippet, &m.Time, &m.Unread, &m.AccountID, &sentAt, &m.MessageID, &m.Folder, &m.UID, &m.HasAttachments); err != nil {
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

type searchJob struct {
	acct     dbAccount
	password string
	folder   string
}

// handleSearch streams progress over SSE rather than blocking until everything's
// done: a mailbox can have 100+ folders, and waiting for a single all-or-nothing
// response left the user staring at "no matches" for as long as the slowest folder
// took, then losing whatever was found if it ran past the time budget.
func (s *Store) handleSearch(w http.ResponseWriter, r *http.Request, owner string) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	since, sinceErr := parseSearchDate(r.URL.Query().Get("since"))
	before, beforeErr := parseSearchDate(r.URL.Query().Get("before"))
	if sinceErr != nil || beforeErr != nil {
		http.Error(w, "since/before must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	folder := r.URL.Query().Get("folder") // optional: restrict to exactly one folder
	folders := r.URL.Query()["folders"]   // optional: restrict to a specific chosen set (repeated param)
	tagIDs := r.URL.Query()["tags"]       // optional: narrow results to mail tagged with any of these
	if query == "" && from == "" && since.IsZero() && before.IsZero() && len(tagIDs) == 0 {
		http.Error(w, "q, from, a date range, or a tag is required", http.StatusBadRequest)
		return
	}
	deep := r.URL.Query().Get("mode") == "deep"
	timeout := lightSearchTimeout
	if deep {
		timeout = deepSearchTimeout
	}

	ids, err := s.ownedAccountIDs(owner, accountFilter(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sendEvent := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	// Tag-only search ("just show me everything tagged X") needs no IMAP round trip at
	// all — tags are local data, so this is a single query against the local cache
	// instead of fanning out across every folder with no real search criteria.
	if query == "" && from == "" && since.IsZero() && before.IsZero() && len(tagIDs) > 0 {
		mails, err := s.searchByTagsOnly(ids, tagIDs)
		if err != nil {
			log.Printf("tag-only search: %v", err)
		}
		sendEvent("progress", map[string]int{"done": 1, "total": 1})
		if len(mails) > 0 {
			sendEvent("match", mails)
		}
		sendEvent("complete", map[string]int{"matches": len(mails)})
		return
	}

	// A "Continue" request names exactly which (account, folder) pairs a previous,
	// timed-out search never got to — see the timedOut handling below. Resuming means
	// searching only those, not re-listing and re-running everything from scratch.
	var jobs []searchJob
	if resume := r.URL.Query().Get("resume"); resume != "" {
		byAccount, err := decodeResumeToken(resume)
		if err != nil {
			http.Error(w, "bad resume token", http.StatusBadRequest)
			return
		}
		for accountID, folders := range byAccount {
			if !allowed[accountID] {
				continue
			}
			acct, password, err := s.loadAccountCreds(accountID)
			if err != nil {
				log.Printf("search account %s: %v", accountID, err)
				continue
			}
			for _, f := range folders {
				jobs = append(jobs, searchJob{acct, password, f})
			}
		}
	} else {
		for _, id := range ids {
			acct, password, err := s.loadAccountCreds(id)
			if err != nil {
				log.Printf("search account %s: %v", id, err)
				continue
			}
			accountFolders := folders // the explicit multi-select list, if one was given
			if len(accountFolders) == 0 && folder != "" {
				accountFolders = []string{folder}
			}
			if len(accountFolders) == 0 {
				names, err := listFoldersForSearch(acct, password)
				if err != nil {
					log.Printf("search account %s: list folders: %v", id, err)
					continue
				}
				accountFolders = names
			}
			for _, f := range accountFolders {
				jobs = append(jobs, searchJob{acct, password, f})
			}
		}
	}

	log.Printf("search %q from=%q: owner=%s mode=%s folder=%q folders=%v accounts=%v folders-to-search=%d", query, from, owner, modeLabel(deep), folder, folders, ids, len(jobs))
	sendEvent("progress", map[string]int{"done": 0, "total": len(jobs)})
	if len(jobs) == 0 {
		sendEvent("complete", map[string]int{"matches": 0})
		return
	}

	type jobResult struct {
		idx   int
		mails []Mail
	}
	resultsCh := make(chan jobResult, len(jobs))
	sem := make(chan struct{}, maxConcurrentFolderSearches)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(idx int, j searchJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mails, err := searchFolder(j.acct, j.password, j.folder, query, from, deep, since, before)
			if err != nil {
				log.Printf("search %s/%s: %v", j.acct.Email, j.folder, err)
			}
			if len(tagIDs) > 0 {
				// Tags are local-only data IMAP knows nothing about, so this can only ever
				// narrow an existing text/sender search — not search by tag alone.
				mails = s.filterByTags(mails, tagIDs)
			}
			// Persist every match as soon as it's found, not just whatever survives the
			// final sort+cap below — mailsFromFetch never sets Mail.AccountID (it's an
			// explicit upsertMails parameter instead), so grouping by that field here
			// would silently bucket everything under "" and fail the accounts(id) FK,
			// which is exactly what made every search result 404 on open.
			if len(mails) > 0 {
				if err := s.upsertMails(j.acct.ID, mails); err != nil {
					log.Printf("cache search result %s/%s: %v", j.acct.Email, j.folder, err)
				}
			}
			resultsCh <- jobResult{idx, mails}
		}(i, j)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	allResults := []Mail{} // not nil — json.Encode(nil slice) is `null`
	completed := make([]bool, len(jobs))
	done := 0
	timedOut := false
	deadline := time.After(timeout)
collect:
	for {
		select {
		case jr, ok := <-resultsCh:
			if !ok {
				break collect
			}
			completed[jr.idx] = true
			done++
			if len(jr.mails) > 0 {
				allResults = append(allResults, jr.mails...)
				sendEvent("match", jr.mails)
			}
			sendEvent("progress", map[string]int{"done": done, "total": len(jobs)})
		case <-deadline:
			timedOut = true
			log.Printf("search %q: hit %s timeout after %d/%d folders", query, timeout, done, len(jobs))
			break collect
		case <-r.Context().Done():
			return // client closed the search / navigated away — stop bothering
		}
	}

	sort.Slice(allResults, func(i, j int) bool { return allResults[i].Date > allResults[j].Date })
	if len(allResults) > searchResultLimit {
		allResults = allResults[:searchResultLimit]
	}

	// timedOut tells the client whether this was a clean finish or a cutoff — without
	// it, stopping one folder short of the total looked identical to finishing, just
	// silently missing whatever that last folder (or the deadline that hit mid-search)
	// might have had. resumeToken names exactly the (account, folder) pairs that never
	// got a result, so "Continue" can pick up just those instead of starting over.
	complete := map[string]any{"matches": len(allResults), "done": done, "total": len(jobs), "timedOut": timedOut}
	if timedOut {
		remaining := map[string][]string{}
		for i, ok := range completed {
			if !ok {
				remaining[jobs[i].acct.ID] = append(remaining[jobs[i].acct.ID], jobs[i].folder)
			}
		}
		if token, err := encodeResumeToken(remaining); err == nil {
			complete["resume"] = token
		}
	}
	sendEvent("complete", complete)
}

func encodeResumeToken(byAccount map[string][]string) (string, error) {
	b, err := json.Marshal(byAccount)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeResumeToken(token string) (map[string][]string, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	var byAccount map[string][]string
	if err := json.Unmarshal(b, &byAccount); err != nil {
		return nil, err
	}
	return byAccount, nil
}

func modeLabel(deep bool) string {
	if deep {
		return "deep"
	}
	return "light"
}

// ownedAccountIDs returns the owner's account ids, optionally restricted to filter
// (a subset previously validated to belong to the owner — e.g. from the account
// filter chips). Empty filter means every account the owner has.
func (s *Store) ownedAccountIDs(owner string, filter []string) ([]string, error) {
	query := "SELECT id FROM accounts WHERE owner_subject = $1"
	args := []any{owner}
	if len(filter) > 0 {
		query += " AND id = ANY($2)"
		args = append(args, filter)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func listFoldersForSearch(acct dbAccount, password string) ([]string, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	watchdog := time.AfterFunc(perFolderSearchTimeout, func() { c.Close() })
	defer watchdog.Stop()
	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	data, err := c.List("", "*", nil).Collect()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(data))
	for i, d := range data {
		names[i] = d.Mailbox
	}
	return names, nil
}

// searchFolder opens its own connection (see maxConcurrentFolderSearches) and
// searches/fetches matches in one folder. deep=false searches Subject/From only
// (HEADER, cheap); deep=true searches the full message TEXT (headers + body). from,
// if set, is ANDed on top as its own From: filter — independent of query, so you can
// filter by sender alone, or narrow a text search down to one sender.
// parseSearchDate parses a YYYY-MM-DD date filter, returning a zero time.Time (which
// the caller treats as "no filter") for an empty string.
func parseSearchDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}

// perFolderSearchTimeout bounds a single folder's entire IMAP round trip (connect,
// login, select, search, fetch). None of imapclient's commands carry a deadline of
// their own — a single unresponsive folder (a server hiccup, a stalled connection
// with no proper TCP reset) could block forever, occupying one of
// maxConcurrentFolderSearches' slots indefinitely. Every other folder still finishes,
// but the *search as a whole* never reaches 100% — it just sits one short of the
// total until handleSearch's own global deadline finally gives up and reports a
// timeout, which looks like (but isn't) the same kind of timeout a server that's
// genuinely just slow on everything would cause. Closing the connection from another
// goroutine after this elapses is the standard way to forcibly unblock a Go I/O call
// that has no deadline of its own — any in-flight Read/Write returns an error
// immediately instead of hanging.
const perFolderSearchTimeout = 12 * time.Second

func searchFolder(acct dbAccount, password, folder, query, from string, deep bool, since, before time.Time) ([]Mail, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	watchdog := time.AfterFunc(perFolderSearchTimeout, func() { c.Close() })
	defer watchdog.Stop()
	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, nil // unselectable mailbox (e.g. a \Noselect parent node) — skip it
	}

	criteria := &imap.SearchCriteria{}
	if query != "" {
		if deep {
			criteria.Text = []string{query}
		} else {
			criteria.Or = [][2]imap.SearchCriteria{{
				{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: query}}},
				{Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: query}}},
			}}
		}
	}
	if from != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: from})
	}
	if !since.IsZero() {
		criteria.SentSince = since
	}
	if !before.IsZero() {
		criteria.SentBefore = before.AddDate(0, 0, 1) // BEFORE is exclusive — push to end of the chosen day
	}

	data, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}
	if len(uids) > searchResultLimit {
		uids = uids[len(uids)-searchResultLimit:] // newest UIDs are highest
	}

	fetchCmd := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{Envelope: true, Flags: true, UID: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true}})
	msgs, err := fetchCmd.Collect()
	if err != nil {
		return nil, err
	}
	mails, _ := mailsFromFetch(acct.ID, folder, msgs)
	return mails, nil
}
