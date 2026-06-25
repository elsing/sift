package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// IMAP access is only granted via the full-mailbox scope — Gmail has no narrower
// IMAP-only scope. email/profile just identify which address was connected.
var googleOAuthScopes = []string{"https://mail.google.com/", "email", "profile"}

func googleOAuthConfig() *oauth2.Config {
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
		Scopes:       googleOAuthScopes,
		Endpoint:     google.Endpoint,
	}
}

// oauthStateStore holds short-lived CSRF state tokens for the in-flight "connect a
// Google account" handshake, mapping state -> owner subject. The callback is a
// redirect from Google, not a same-session authenticated request, so there's nothing
// else to recover the owner from at that point.
type oauthStateStore struct {
	mu     sync.Mutex
	states map[string]oauthState
}

type oauthState struct {
	owner     string
	expiresAt time.Time
}

func newOAuthStateStore() *oauthStateStore {
	return &oauthStateStore{states: make(map[string]oauthState)}
}

func (o *oauthStateStore) put(owner string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	state := randomID()
	o.states[state] = oauthState{owner: owner, expiresAt: time.Now().Add(10 * time.Minute)}
	return state
}

func (o *oauthStateStore) take(state string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.states[state]
	delete(o.states, state)
	if !ok || time.Now().After(s.expiresAt) {
		return "", false
	}
	return s.owner, true
}

// GoogleOAuthRoutes registers the "Connect Google Account" flow — a second, separate
// OAuth dance from the app's own OIDC login, used to link a Gmail account by OAuth
// instead of an app password. Requires its own Google Cloud OAuth client (see
// .env.example); routes 400 with a clear message if that isn't configured rather than
// failing obscurely deeper in the flow.
func (s *Store) GoogleOAuthRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/accounts/oauth/google/start", func(w http.ResponseWriter, r *http.Request) {
		cfg := googleOAuthConfig()
		if cfg == nil {
			http.Error(w, "Google OAuth isn't set up on this server yet (missing GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET)", http.StatusBadRequest)
			return
		}
		state := s.oauthStates.put(ownerSubject(r))
		url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
		http.Redirect(w, r, url, http.StatusFound)
	})
	mux.HandleFunc("GET /api/accounts/oauth/google/callback", s.handleGoogleOAuthCallback)
}

type googleUserInfo struct {
	Email string `json:"email"`
}

func (s *Store) handleGoogleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	cfg := googleOAuthConfig()
	if cfg == nil {
		http.Error(w, "Google OAuth isn't set up on this server", http.StatusBadRequest)
		return
	}
	owner, ok := s.oauthStates.take(r.URL.Query().Get("state"))
	if !ok {
		http.Error(w, "expired or invalid OAuth state — please try connecting again", http.StatusBadRequest)
		return
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		http.Redirect(w, r, "/?oauthError="+errMsg, http.StatusFound)
		return
	}

	ctx := context.Background()
	tok, err := cfg.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		log.Printf("google oauth exchange: %v", err)
		http.Error(w, "couldn't complete Google sign-in: "+err.Error(), http.StatusBadGateway)
		return
	}
	if tok.RefreshToken == "" {
		// Happens if the user has already granted this app consent before and Google
		// skips re-issuing a refresh token — AccessTypeOffline+prompt=consent above is
		// meant to prevent this, but a stale grant from testing earlier can still hit it.
		http.Error(w, "Google didn't return a refresh token — revoke this app's access at https://myaccount.google.com/permissions and try connecting again", http.StatusBadGateway)
		return
	}

	email, err := fetchGoogleEmail(ctx, cfg, tok)
	if err != nil {
		log.Printf("google oauth userinfo: %v", err)
		http.Error(w, "couldn't read your Google account email: "+err.Error(), http.StatusBadGateway)
		return
	}

	encRefresh, err := s.crypto.encrypt(tok.RefreshToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	encAccess, err := s.crypto.encrypt(tok.AccessToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	id := randomID()
	_, err = s.db.Exec(
		`INSERT INTO accounts (id, owner_subject, email, host, port, username, oauth_provider, oauth_refresh_token_enc, oauth_access_token_enc, oauth_token_expiry)
		 VALUES ($1, $2, $3, 'imap.gmail.com', 993, $3, 'google', $4, $5, $6)`,
		id, owner, email, encRefresh, encAccess, tok.Expiry,
	)
	if err != nil {
		log.Printf("add google account %s: insert: %v", email, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := s.syncAccount(id); err != nil {
		log.Printf("add google account %s: initial sync failed: %v", email, err)
		s.db.Exec("UPDATE accounts SET last_sync_error = $1 WHERE id = $2", err.Error(), id)
	}
	s.cleanupMockMail()
	s.watchAccount(id)
	http.Redirect(w, r, "/", http.StatusFound)
}

func fetchGoogleEmail(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (string, error) {
	client := cfg.Client(ctx, tok)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo returned %d: %s", resp.StatusCode, body)
	}
	var info googleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	if info.Email == "" {
		return "", fmt.Errorf("no email in Google userinfo response")
	}
	return info.Email, nil
}

// refreshGoogleAccessToken exchanges a stored refresh token for a fresh access token.
// Google refresh tokens don't expire under normal use (only on revocation or ~6 months
// inactivity), so there's no refresh-token rotation to persist here — just the new
// access token and its expiry.
func refreshGoogleAccessToken(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	cfg := googleOAuthConfig()
	if cfg == nil {
		return nil, fmt.Errorf("google oauth not configured")
	}
	src := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken})
	return src.Token()
}
