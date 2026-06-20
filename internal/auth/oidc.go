// Package auth handles app login via OIDC (Authentik). This is login to the
// app itself, separate from whatever OAuth a connected mailbox account uses
// later — those are unrelated credentials.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
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

	db         *sql.DB
	sessionTTL time.Duration
}

const sessionCookie = "session"
const stateCookie = "oidc_state"

// New sets up OIDC login backed by Postgres-stored sessions, so logins survive app
// restarts. sessionTTL controls how long a login lasts before requiring re-auth.
// Assumes db.Migrate has already been run by the caller.
func New(ctx context.Context, db *sql.DB, issuer, clientID, clientSecret, redirectURL string, sessionTTL time.Duration) (*Auth, error) {
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
		verifier:   provider.Verifier(&oidc.Config{ClientID: clientID}),
		db:         db,
		sessionTTL: sessionTTL,
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
	expiresAt := time.Now().Add(a.sessionTTL)
	if _, err := a.db.Exec(
		"INSERT INTO sessions (id, subject, email, name, expires_at) VALUES ($1, $2, $3, $4, $5)",
		sessionID, user.Subject, user.Email, user.Name, expiresAt,
	); err != nil {
		http.Error(w, "couldn't create session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sessionID, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(a.sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		a.db.Exec("DELETE FROM sessions WHERE id = $1", ck.Value)
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

		var user User
		var expiresAt time.Time
		row := a.db.QueryRow("SELECT subject, email, name, expires_at FROM sessions WHERE id = $1", ck.Value)
		if err := row.Scan(&user.Subject, &user.Email, &user.Name, &expiresAt); err != nil {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		if time.Now().After(expiresAt) {
			a.db.Exec("DELETE FROM sessions WHERE id = $1", ck.Value)
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}

		ctx := context.WithValue(r.Context(), userCtxKey{}, user)
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
