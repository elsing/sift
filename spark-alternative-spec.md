# Project: Spark Alternative — Phase 1 (UI Skeleton + Gesture Feel)

## Goal
Build a better email client than Spark. The two real complaints with Spark:
1. **Search is bad** — slow, low-relevance, no typo tolerance.
2. **Auto-sort/detection is weak** — categorization of incoming mail isn't smart enough.

UI chrome and "native feel" are secondary, but gesture interactions (swipe-to-archive, swipe-to-delete) need to be evaluated before committing to a platform.

## Phase 1 scope (THIS BUILD)
Do **not** build real email integration yet. This phase is purely a **UI/UX skeleton** to test how swipe gestures and list interactions feel as a PWA/web app, before deciding whether the gesture/haptic gap vs native is acceptable.

Deliverable: a mock inbox screen with:
- A scrollable list of fake email rows (mock data — sender, subject, snippet, timestamp, unread dot)
- Swipe gestures on each row:
  - Swipe right → reveal "Archive" action (green), commits on full swipe or button tap
  - Swipe left → reveal "Delete" action (red), commits on full swipe or button tap
  - Should NOT trigger near the very left/right screen edges (avoid conflicting with iOS edge-swipe back/app-switcher gestures)
- Smooth momentum/rubber-band feel on the swipe (not just a binary toggle)
- Pull-to-refresh gesture at top of list (mock — just resets the fake data)
- Basic "mark as read/unread" tap behavior
- Designed mobile-first, intended to be added to iOS home screen as a PWA (full-screen, no browser chrome)

Explicitly **out of scope** for this phase: auth, real IMAP/Gmail sync, search, auto-sort/classification, push notifications, backend of any kind. Pure frontend, pure mock data, gesture feel is the only thing being evaluated.

## Tech preferences
- Plain web stack (HTML/CSS/JS or React) — whatever renders cleanly as an installable PWA
- Touch handling via native touch events or a lightweight library (e.g. Framer Motion / Hammer.js) — pick whichever gives the smoothest result
- No backend, no build complexity — should run and be testable on an iPhone via Safari "Add to Home Screen" within minutes

## Context for later phases (not part of this build, just for awareness)
- **Search fix**: planned as a backend concern — sync mail into Postgres (full-text search / tsvector) or a dedicated search engine (Meilisearch/Typesense), not a client-side problem.
- **Auto-sort fix**: planned as a backend classification layer — rules-based heuristics and/or lightweight LLM classification on ingest.
- **Backend language**: Go (existing stack/preference).
- **Push notifications**: iOS 16.4+ supports web push for home-screen PWAs, so native app is not required just for this.
- **Native app tradeoffs already considered**: native gives better gesture latency, haptic feedback (Taptic Engine — no web equivalent), and no edge-gesture conflicts. Web/PWA is "90% there" on gesture feel but will never fully match native. This phase is to actually feel that gap firsthand before deciding.
- **Distribution note**: Apple Developer Program is $99/year, required for TestFlight and App Store regardless — free Xcode sideloading exists but certs expire every 7 days. Not relevant to this phase since we're staying web-based for now.

## Success criteria for this phase
Elliot can add it to his iOS home screen, swipe through ~15-20 mock emails, and have a clear gut feeling on whether the gesture/haptic gap vs native is a dealbreaker or acceptable — informing whether to continue with PWA or pivot to native (SwiftUI).

## Later additions
This file is frozen at Phase 1. Built since: full manual + smart auto-tagging — see [docs/smart-tagging.md](docs/smart-tagging.md).
