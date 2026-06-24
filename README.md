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
- Same swipe-down bottom-sheet UI as the folder picker — drag the grabber or tap the dimmed area above it to dismiss

**Tags**
- Manual tagging, plus folder→tag rules (move mail into a folder, it gets tagged automatically)
- Per-tag destination folder, with auto-move out of the inbox after a configurable delay
- Per-tag notification toggle — mute pushes for tags you trust to sort themselves

**Smart auto-tagging**
- Scores incoming mail against your own tagging history (sender, domain, subject) and either applies or suggests a tag
- Two modes: *Review* (everything's a suggestion) or *Full-auto* (confident matches apply immediately)
- **Scan for tags** — a one-off bootstrap pass over existing mail (all folders, just the inbox, or a hand-picked set), including a "you've already filed this elsewhere — make it a tag?" detector for folders you sorted by hand
- **Misplaced mail** — finds mail that's tagged but not sitting in that tag's folder, with a one-tap fix
- See [docs/smart-tagging.md](docs/smart-tagging.md) for the scoring details and design tradeoffs

**Smart spam detection**
- A real heuristic engine — SPF/DKIM/DMARC, sender/reply-to/return-path mismatches, display-name spoofing, content phrase/punctuation/link checks — not just history matching, so it actually catches a sender you've never seen before
- Same Review/Full-auto mode choice as tagging, kept as its own separate setting and its own menu — spam is its own concept, not a sub-feature of tagging, even though it shares the same plumbing underneath
- Spam mail never auto-loads remote images regardless of your global image setting, and never gets cached if you do load it — both reverse instantly the moment it's marked "Not spam"
- Every opened email shows a quiet, always-visible score/SPF/DKIM/DMARC/reasons readout at the bottom of the reader — purely diagnostic, never triggers scoring on its own
- **Scan for spam** — the same one-off bootstrap pattern as tagging's scan, including treating whatever's already sorted into Junk/Trash as training evidence (still scored for real, never a silent bypass)
- Suggestions and full-auto activity are grouped by sender, not flattened — a bulk spam run from one sender collapses into one group instead of dozens of rows
- See [docs/spam-detection.md](docs/spam-detection.md) for the full scoring breakdown

**Auto-tag activity**
- One shared audit/undo panel for both Smart Tagging and Smart Spam's full-auto decisions — a Tagging/Spam toggle switches which one you're looking at
- One-tap undo, which also teaches the relevant scorer not to repeat the mistake

**Folders**
- Browse, move mail between folders, multi-account folder pickers
- Real folder management — create, rename, delete — not just browsing
- Folder browsing is database-backed with zero IMAP round trips on a normal visit — a periodic background sync keeps every folder current, live IMAP is reserved for an explicit pull-to-refresh; see [docs/folder-mail-caching.md](docs/folder-mail-caching.md)
- A manual refresh button in the folder picker for an on-demand live check, without re-rendering anything if nothing actually changed

**Other**
- Web push notifications (works as a home-screen PWA, no native app required)
- Self-hosted OIDC login (built against Authentik, any standard OIDC provider should work)
- Dry-run mode for testing without actually mutating anything on the server
- Light/dark theme, accent color picker
- **Backup** — one-tap export/import of everything needed for a copy-paste deployment or disaster recovery: connected accounts, tags, folder assignments, smart-tagging's learned history, spam history, settings, and personalisation — not just tags. Optional passphrase encrypts the file (it contains plaintext account credentials by necessity); see [docs/backup.md](docs/backup.md)

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

Every push to `main` (and every `vX.Y.Z` tag) builds and publishes the image to
`ghcr.io/elsing/sift` via `.github/workflows/docker-publish.yml`. To run from that
instead of building locally: set `SIFT_IMAGE_TAG` in `.env` (defaults to `latest`),
then `docker compose pull && docker compose up -d` — `task up`/`task restart` always
build from source regardless.

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
internal/api/      HTTP handlers + business logic (mail, tags, search, smart-tagging, spam detection, folders, accounts)
internal/auth/     OIDC login/session handling
internal/db/       migrations + migration runner
web/               the entire frontend — index.html, style.css, js/ (one module per concern)
docs/              design docs for the less self-explanatory subsystems
scripts/genvapid/  one-shot CLI to generate VAPID keys for setup-env.sh
```

No build step on the frontend — `web/` is served as-is, so a code change there just
needs a browser refresh (or `task restart` if you changed Go code).
