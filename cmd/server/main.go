package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"sift/internal/api"
	"sift/internal/auth"
	"sift/internal/db"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	ctx := context.Background()

	conn, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if err := db.Migrate(conn); err != nil {
		log.Fatal(err)
	}

	store, err := api.NewStore(conn)
	if err != nil {
		log.Fatal(err)
	}

	sessionDays := 30
	if v := os.Getenv("SESSION_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			sessionDays = n
		}
	}

	a, err := auth.New(ctx, conn, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_CLIENT_ID"),
		os.Getenv("OIDC_CLIENT_SECRET"), os.Getenv("OIDC_REDIRECT_URL"), time.Duration(sessionDays)*24*time.Hour)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	a.Routes(mux)

	protected := http.NewServeMux()
	store.Routes(protected)
	store.AccountsRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	store.PushRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	store.SearchRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	store.TagsRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	store.TagHistoryRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	// no-cache (not no-store) so the browser always revalidates via ETag/Last-Modified
	// instead of serving a stale copy outright — this app gets redeployed often, and a
	// PWA on a home screen is especially prone to caching static assets aggressively.
	static := http.FileServer(http.Dir("web"))
	protected.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	// iOS's "Add to Home Screen" fetches the manifest and touch-icon outside the
	// normal page-load context — that fetch doesn't reliably carry the session
	// cookie, so gating these behind login meant it got a redirect-to-login HTML page
	// instead of an image, and silently fell back to a blank/generic icon. None of
	// these are sensitive (just branding assets), so they're served unauthenticated,
	// registered directly on the outer mux ahead of the "/" catch-all.
	for _, path := range []string{"/icon.svg", "/icon-180.png", "/icon-512.png", "/manifest.json"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			static.ServeHTTP(w, r)
		})
	}
	mux.Handle("/", a.Require(protected))

	store.StartWatching(ctx)

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
