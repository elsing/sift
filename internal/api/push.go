package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// PushRoutes registers Web Push subscription endpoints. Notification *delivery* (the
// actual SendNotification calls) happens from idleOnce in realtime.go, triggered by
// real new mail rather than any HTTP request.
func (s *Store) PushRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/push/public-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"publicKey": os.Getenv("VAPID_PUBLIC_KEY")})
	})
	mux.HandleFunc("POST /api/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		s.handlePushSubscribe(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/push/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		s.handlePushUnsubscribe(w, r, ownerSubject(r))
	})
}

func (s *Store) handlePushSubscribe(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	_, err := s.db.Exec(
		`INSERT INTO push_subscriptions (id, owner_subject, endpoint, p256dh, auth) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (endpoint) DO UPDATE SET owner_subject = excluded.owner_subject, p256dh = excluded.p256dh, auth = excluded.auth`,
		randomID(), owner, req.Endpoint, req.Keys.P256dh, req.Keys.Auth,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// send a real push right away so enabling notifications is provable, not just "no error shown"
	go s.notifyOwner(owner, "Notifications enabled", "You'll get a push here when new mail arrives.", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec("DELETE FROM push_subscriptions WHERE owner_subject = $1 AND endpoint = $2", owner, req.Endpoint); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notifyOwner pushes a browser notification to every device an account's owner has
// subscribed from. Subscriptions that the push service reports as gone (expired/revoked,
// HTTP 404/410) are deleted so we stop wasting requests on them.
func (s *Store) notifyOwner(owner, title, body, mailID string) {
	rows, err := s.db.Query("SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE owner_subject = $1", owner)
	if err != nil {
		log.Printf("notify owner: %v", err)
		return
	}
	type sub struct{ id, endpoint, p256dh, auth string }
	var subs []sub
	for rows.Next() {
		var s sub
		if err := rows.Scan(&s.id, &s.endpoint, &s.p256dh, &s.auth); err != nil {
			rows.Close()
			log.Printf("notify owner: %v", err)
			return
		}
		subs = append(subs, s)
	}
	log.Printf("notify owner %s: sending %q to %d subscription(s)", owner, title, len(subs))

	payload, _ := json.Marshal(map[string]string{"title": title, "body": body, "mailId": mailID})
	for _, sb := range subs {
		resp, err := webpush.SendNotification(payload, &webpush.Subscription{
			Endpoint: sb.endpoint,
			Keys:     webpush.Keys{P256dh: sb.p256dh, Auth: sb.auth},
		}, &webpush.Options{
			// Apple's push service rejects a sub claim using a reserved/non-routable TLD
			// (e.g. .local) outright — Google/Mozilla don't check it, so this only showed
			// up against Apple's stricter validator. Use a real domain.
			Subscriber:      "elliot@singermail.uk",
			VAPIDPublicKey:  os.Getenv("VAPID_PUBLIC_KEY"),
			VAPIDPrivateKey: os.Getenv("VAPID_PRIVATE_KEY"),
			TTL:             60,
		})
		if err != nil {
			log.Printf("send push to %s: %v", sb.endpoint, err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Printf("send push to %s: status %s, body %q", sb.endpoint, resp.Status, respBody)
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			s.db.Exec("DELETE FROM push_subscriptions WHERE id = $1", sb.id)
		}
	}
}
