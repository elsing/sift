package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// broadcaster fans out "new mail" notifications to connected SSE clients (see
// handleEvents). One process-wide instance is enough for a personal, single-user app.
type broadcaster struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{clients: make(map[chan string]struct{})}
}

func (b *broadcaster) subscribe() chan string {
	ch := make(chan string, 4)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *broadcaster) publish(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default: // a slow/stuck client shouldn't block everyone else
		}
	}
}

// handleEvents is a Server-Sent Events stream: the client opens it once and gets a
// "mail" event whenever an IDLE watcher (below) sees new mail arrive. Simpler than a
// WebSocket since this app only ever needs to push one way.
func (s *Store) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// no "Connection: keep-alive" — that's a connection-specific header forbidden under
	// HTTP/2 (which a proxy/tunnel in front of this app may negotiate with the browser),
	// and setting it can trip a protocol error that kills the stream outright.

	ch := s.broadcaster.subscribe()
	defer s.broadcaster.unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: %s\ndata: {}\n\n", msg)
			flusher.Flush()
		case <-time.After(25 * time.Second):
			fmt.Fprint(w, ": keepalive\n\n") // comment line; stops proxies/browsers timing out an idle stream
			flusher.Flush()
		}
	}
}

// StartWatching launches one persistent IMAP IDLE connection per existing account.
// ctx governs every watcher's lifetime — cancel it (e.g. on process shutdown) to stop
// them all. Call once at startup; watchAccount/stopWatching handle accounts added or
// removed afterwards.
func (s *Store) StartWatching(ctx context.Context) {
	s.watchCtx = ctx
	rows, err := s.db.Query("SELECT id FROM accounts")
	if err != nil {
		log.Printf("start watching: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Printf("start watching: %v", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.watchAccount(id)
	}
}

// watchAccount runs (and indefinitely restarts, with backoff) an IDLE loop for one
// account. Safe to call again for an account already being watched — it replaces the
// old watcher rather than running two.
func (s *Store) watchAccount(accountID string) {
	if s.watchCtx == nil {
		return // StartWatching hasn't run (e.g. under test); nothing to attach the watcher to
	}
	ctx, cancel := context.WithCancel(s.watchCtx)

	s.watchMu.Lock()
	if existing, ok := s.watchCancels[accountID]; ok {
		existing()
	}
	s.watchCancels[accountID] = cancel
	s.watchMu.Unlock()

	go func() {
		backoff := 5 * time.Second
		for {
			if err := s.idleOnce(ctx, accountID); err != nil {
				log.Printf("idle %s: %v", accountID, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
		}
	}()
}

// stopWatching cancels an account's IDLE loop, e.g. when the account is removed.
func (s *Store) stopWatching(accountID string) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if cancel, ok := s.watchCancels[accountID]; ok {
		cancel()
		delete(s.watchCancels, accountID)
	}
}

// idleOnce holds one IMAP IDLE connection open until new mail arrives or the
// connection drops, syncs once, and returns; watchAccount's loop reconnects for the
// next round. Doing one "session" per call keeps connection/credential errors and
// reconnect backoff in a single place.
func (s *Store) idleOnce(ctx context.Context, accountID string) error {
	acct, password, err := s.loadAccountCreds(accountID)
	if err != nil {
		return err
	}

	newMail := make(chan struct{}, 1)
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				log.Printf("idle %s: mailbox update, numMessages=%v", accountID, data.NumMessages)
				if data.NumMessages != nil {
					select {
					case newMail <- struct{}{}:
					default:
					}
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if !c.Caps().Has(imap.CapIdle) {
		return fmt.Errorf("server doesn't support IDLE; can't watch for live mail")
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("select inbox: %w", err)
	}

	idleCmd, err := c.Idle()
	if err != nil {
		return fmt.Errorf("idle: %w", err)
	}
	log.Printf("idle %s: connected, watching INBOX", accountID)

	select {
	case <-ctx.Done():
		idleCmd.Close()
		return nil
	case <-newMail:
		idleCmd.Close()
	}

	log.Printf("idle %s: new mail signal, syncing", accountID)
	mails, err := s.syncAccount(accountID)
	if err != nil {
		return fmt.Errorf("sync after idle: %w", err)
	}
	s.broadcaster.publish("mail")
	s.notifyNewMail(accountID, mails)
	return nil
}

// notifyNewMail sends a browser push naming the mail that just arrived. mails is
// whatever this sync round fetched from IMAP — UID increases monotonically per
// mailbox, so the highest-UID mail in that batch is reliably "what's new", unlike
// sorting the whole mailbox by a parsed Date header (which a backdated or unparsed
// message can throw off). Best-effort: a lookup/send failure here shouldn't fail the
// IDLE loop, it just means no notification this round.
func (s *Store) notifyNewMail(accountID string, mails []Mail) {
	if len(mails) == 0 {
		return
	}
	newest := mails[0]
	for _, m := range mails[1:] {
		if m.UID > newest.UID {
			newest = m
		}
	}

	var owner string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&owner); err != nil {
		log.Printf("notify new mail %s: %v", accountID, err)
		return
	}
	s.notifyOwner(owner, newest.Sender, newest.Subject, newest.ID)
}
