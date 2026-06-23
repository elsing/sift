package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
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
	store.GoogleOAuthRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	store.ImageCacheRoutes(protected, func(r *http.Request) string {
		return auth.UserFromContext(r.Context()).Subject
	})
	store.SpamRoutes(protected, func(r *http.Request) string {
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
	// "Revalidate" turned out not to be good enough in practice (style.css and js/main.js
	// have both shown up stale on a phone after a deploy, no-cache header notwithstanding —
	// browsers aren't required to actually round-trip a revalidation request every time,
	// and ES module imports in particular are sticky about caching across reloads). A
	// query-string version on the two thing that change every deploy forces a genuinely
	// new URL instead of trusting cache revalidation to behave — computed once at
	// startup, which already changes every deploy since the container restarts.
	cssVersion := strconv.FormatInt(time.Now().Unix(), 10)
	indexHTML, err := os.ReadFile("web/index.html")
	if err != nil {
		log.Fatal(err)
	}
	indexHTML = []byte(strings.NewReplacer(
		`href="style.css"`, `href="style.css?v=`+cssVersion+`"`,
		`src="js/main.js"`, `src="js/main.js?v=`+cssVersion+`"`,
		`src="js/vendor/pulltorefresh.js"`, `src="js/vendor/pulltorefresh.js?v=`+cssVersion+`"`,
	).Replace(string(indexHTML)))

	static := http.FileServer(http.Dir("web"))
	protected.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(indexHTML)
			return
		}
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
