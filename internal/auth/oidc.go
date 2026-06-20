// Package auth handles app login via OIDC (Authentik). This is login to the
// app itself, separate from whatever OAuth a connected mailbox account uses
// later — those are unrelated credentials.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type User struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
}

type Auth struct {
	provider    *oidc.Provider
	oauthConfig oauth2.Config
	verifier    *oidc.IDTokenVerifier

	mu       sync.Mutex
	sessions map[string]session // ponytail: in-memory sessions, move to Postgres if surviving restarts matters
}

type session struct {
	user      User
	expiresAt time.Time
}

const sessionCookie = "session"
const stateCookie = "oidc_state"
const sessionTTL = 7 * 24 * time.Hour

func New(ctx context.Context, issuer, clientID, clientSecret, redirectURL string) (*Auth, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	return &Auth{
		provider: provider,
		oauthConfig: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		sessions: make(map[string]session),
	}, nil
}

func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, a.oauthConfig.AuthCodeURL(state), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	stateCk, err := r.Cookie(stateCookie)
	if err != nil || stateCk.Value == "" || stateCk.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	token, err := a.oauthConfig.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, "id_token invalid: "+err.Error(), http.StatusUnauthorized)
		return
	}

	var user User
	if err := idToken.Claims(&user); err != nil {
		http.Error(w, "bad claims: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sessionID := randomToken()
	a.mu.Lock()
	a.sessions[sessionID] = session{user: user, expiresAt: time.Now().Add(sessionTTL)}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sessionID, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, ck.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

type userCtxKey struct{}

// Require redirects to login when there's no valid session, otherwise calls next
// with the logged-in User attached to the request context.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		a.mu.Lock()
		sess, ok := a.sessions[ck.Value]
		a.mu.Unlock()
		if !ok || time.Now().After(sess.expiresAt) {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey{}, sess.user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// UserFromContext returns the logged-in User stashed by Require, or zero value if absent.
func UserFromContext(ctx context.Context) User {
	u, _ := ctx.Value(userCtxKey{}).(User)
	return u
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the system is broken beyond recovery
	}
	return hex.EncodeToString(b)
}
