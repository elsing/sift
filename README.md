# Sift

A self-hosted email client built around two complaints with every other inbox app:
search that's actually good, and sorting that doesn't need babysitting.

Mobile-first PWA frontend (installs straight to your home screen, no App Store), Go
backend, Postgres, your own IMAP accounts, your own self-hosted login.

## Features

**Inbox**
- Swipe gestures (archive, delete, move, mark read, tag — each side configurable)
- Pull-to-refresh, infinite scroll, live updates over SSE (no polling)
- Mail with a shared tag groups into one collapsible row instead of cluttering the list
- Works across multiple IMAP accounts at once, with an account filter

**Search**
- Header-only ("light") search-as-you-type, with an explicit full-body ("deep") search as a deliberate next step
- Scope to one folder, a hand-picked set of folders, or everything
- Filter by tag — local data, so this skips IMAP entirely and is instant
- Resumable if a many-folder mailbox times out partway through

**Tags**
- Manual tagging, plus folder→tag rules (move mail into a folder, it gets tagged automatically)
- Per-tag destination folder, with auto-move out of the inbox after a configurable delay
- Per-tag notification toggle — mute pushes for tags you trust to sort themselves

**Smart auto-tagging**
- Scores incoming mail against your own tagging history (sender, domain, subject) and either applies or suggests a tag
- Two modes: *Review* (everything's a suggestion) or *Full-auto* (confident matches apply immediately)
- **Auto-tag activity** panel — full-auto's own audit trail, with one-tap undo (which also teaches it not to repeat the mistake)
- **Scan for tags** — a one-off bootstrap pass over existing mail (all folders, just the inbox, or a hand-picked set), including a "you've already filed this elsewhere — make it a tag?" detector for folders you sorted by hand
- **Misplaced mail** — finds mail that's tagged but not sitting in that tag's folder, with a one-tap fix
- See [docs/smart-tagging.md](docs/smart-tagging.md) for the scoring details and design tradeoffs

**Folders**
- Browse, move mail between folders, multi-account folder pickers
- Real folder management — create, rename, delete — not just browsing

**Other**
- Web push notifications (works as a home-screen PWA, no native app required)
- Self-hosted OIDC login (built against Authentik, any standard OIDC provider should work)
- Dry-run mode for testing without actually mutating anything on the server
- Light/dark theme, accent color picker

## Quick start

Requires Docker, an IMAP-accessible mailbox to connect, and an OIDC provider (e.g.
[Authentik](https://goauthentik.io/)) for login.

```bash
cp .env.example .env
./setup-env.sh        # fills in random secrets (Postgres password, encryption key, VAPID keys)
# then edit .env: fill in OIDC_ISSUER / OIDC_CLIENT_ID / OIDC_CLIENT_SECRET / OIDC_REDIRECT_URL
task up                # docker compose up -d --build
```

Open `http://<host>:8080`. On iOS/Android, "Add to Home Screen" for the full PWA experience
(push notifications require this on iOS).

Other useful commands (see `Taskfile.yml`):

```bash
task restart   # rebuild + restart just the app container, after a code change
task logs      # tail app logs
task build     # compile the Go server without Docker, for a quick syntax check
```

## Tech stack

- **Backend**: Go, `database/sql` + pgx, raw SQL (no ORM) — `internal/api`, `internal/auth`, `internal/db`
- **Frontend**: vanilla JS ES modules, no build step, no framework — `web/js`
- **Database**: Postgres, migrations in `internal/db/migrations`
- **Mail protocol**: IMAP via [go-imap/v2](https://github.com/emersion/go-imap)
- **Auth**: OIDC (designed against self-hosted Authentik)
- **Push**: Web Push (VAPID) via [webpush-go](https://github.com/SherClockHolmes/webpush-go)
- **Deploy**: Docker Compose (app + Postgres)

## Project structure

```
cmd/server/        entrypoint — wires up DB, auth, routes, starts the IMAP IDLE watchers
internal/api/      HTTP handlers + business logic (mail, tags, search, smart-tagging, folders, accounts)
internal/auth/     OIDC login/session handling
internal/db/       migrations + migration runner
web/               the entire frontend — index.html, style.css, js/ (one module per concern)
docs/              design docs for the less self-explanatory subsystems
scripts/genvapid/  one-shot CLI to generate VAPID keys for setup-env.sh
```

No build step on the frontend — `web/` is served as-is, so a code change there just
needs a browser refresh (or `task restart` if you changed Go code).
