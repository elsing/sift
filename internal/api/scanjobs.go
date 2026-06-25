package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// scanJobPollInterval is how often a subscribed SSE connection re-reads a job's
// progress row — frequent enough to feel live, far cheaper than the actual scan work
// it's watching.
const scanJobPollInterval = 700 * time.Millisecond

// findRunningScanJob returns the id of an already-running job for this exact
// account+kind, if any — reusing it instead of starting a second overlapping scan is
// the whole reason this lookup exists, not just an optimization.
func (s *Store) findRunningScanJob(accountID, kind string) (string, bool) {
	var id string
	err := s.db.QueryRow(
		"SELECT id FROM scan_jobs WHERE account_id = $1 AND kind = $2 AND status = 'running' ORDER BY created_at DESC LIMIT 1",
		accountID, kind,
	).Scan(&id)
	return id, err == nil
}

// findRunningScanJobForOwner is findRunningScanJob's owner-wide counterpart — what a
// panel reopening after a reload uses to find "is anything still going" without
// already knowing which account it was started against. Also doubles as the dedup
// check for jobs that have no single account at all (a cleanup across every account),
// in which case accountID comes back "".
func (s *Store) findRunningScanJobForOwner(ownerSubject, kind string) (jobID, accountID string, found bool) {
	var acctID *string
	err := s.db.QueryRow(
		"SELECT id, account_id FROM scan_jobs WHERE owner_subject = $1 AND kind = $2 AND status = 'running' ORDER BY created_at DESC LIMIT 1",
		ownerSubject, kind,
	).Scan(&jobID, &acctID)
	if acctID != nil {
		accountID = *acctID
	}
	return jobID, accountID, err == nil
}

// startScanJob creates the scan_jobs row a fresh run will report progress into.
// accountID is "" for an owner-wide job with no single account to scope to (stored as
// NULL — scan_jobs.account_id is a real foreign key, so an empty string would either
// violate it or silently match nothing once NOT NULL was dropped).
func (s *Store) startScanJob(ownerSubject, accountID, kind string) (string, error) {
	id := randomID()
	var accountIDArg any
	if accountID != "" {
		accountIDArg = accountID
	}
	_, err := s.db.Exec(
		"INSERT INTO scan_jobs (id, owner_subject, account_id, kind) VALUES ($1, $2, $3, $4)",
		id, ownerSubject, accountIDArg, kind,
	)
	return id, err
}

// runScanJob runs run on a server-lifetime context (s.watchCtx, the same root every
// IMAP IDLE watcher already runs on) — deliberately NOT the HTTP request that
// triggered it, so a reloaded/closed browser tab no longer kills the scan itself, only
// the one SSE connection that happened to be watching it. Cancellation is now an
// explicit action (cancelScanJob) rather than an implicit side effect of the
// connection dropping.
func (s *Store) runScanJob(jobID, kind string, run func(ctx context.Context, onProgress func(done, total int)) (any, error)) {
	base := s.watchCtx
	if base == nil {
		base = context.Background() // StartWatching hasn't run (e.g. under test) — same fallback realtime.go uses
	}
	ctx, cancel := context.WithCancel(base)
	s.scanJobMu.Lock()
	s.scanJobCancels[jobID] = cancel
	s.scanJobMu.Unlock()
	defer func() {
		s.scanJobMu.Lock()
		delete(s.scanJobCancels, jobID)
		s.scanJobMu.Unlock()
		cancel()
	}()

	summary, err := run(ctx, func(done, total int) {
		s.db.Exec("UPDATE scan_jobs SET done = $1, total = $2, updated_at = now() WHERE id = $3", done, total, jobID)
	})

	switch {
	case ctx.Err() != nil:
		s.db.Exec("UPDATE scan_jobs SET status = 'cancelled', updated_at = now() WHERE id = $1", jobID)
	case err != nil:
		s.db.Exec("UPDATE scan_jobs SET status = 'error', error = $1, updated_at = now() WHERE id = $2", err.Error(), jobID)
	default:
		b, jsonErr := json.Marshal(summary)
		if jsonErr != nil {
			log.Printf("marshal scan job %s summary: %v", jobID, jsonErr)
			b = []byte("null")
		}
		s.db.Exec("UPDATE scan_jobs SET status = 'done', summary = $1, updated_at = now() WHERE id = $2", b, jobID)
	}
}

// cancelScanJob stops a running job's context — see runScanJob's own comment on why
// this has to be an explicit action now rather than just closing the SSE connection.
// A no-op if the job isn't running in this process (already finished, or — after a
// restart — already marked 'interrupted' by NewStore rather than tracked here).
func (s *Store) cancelScanJob(jobID string) {
	s.scanJobMu.Lock()
	cancel, ok := s.scanJobCancels[jobID]
	s.scanJobMu.Unlock()
	if ok {
		cancel()
	}
}

// scanJobSnapshot is one poll of a job's current row — exactly what subscribeScanJob
// streams to an SSE connection on each tick.
type scanJobSnapshot struct {
	Status  string
	Done    int
	Total   int
	Summary json.RawMessage
	Error   string
}

func (s *Store) scanJobSnapshotByID(jobID string) (scanJobSnapshot, error) {
	var snap scanJobSnapshot
	var summary, errMsg *string
	err := s.db.QueryRow(
		"SELECT status, done, total, summary::text, error FROM scan_jobs WHERE id = $1", jobID,
	).Scan(&snap.Status, &snap.Done, &snap.Total, &summary, &errMsg)
	if summary != nil {
		snap.Summary = json.RawMessage(*summary)
	}
	if errMsg != nil {
		snap.Error = *errMsg
	}
	return snap, err
}

// handleOwnerJobSSE is the shared SSE wiring every owner-wide (no single account) job
// action uses: reattach to an already-running job of this kind for this owner, or
// start one and run it on its own server-lifetime context, then stream progress until
// done — same persistence/reload-survival every other job gets (runScanJob, this
// file), just without an account_id to scope by. Account-scoped jobs (the folder
// scans, apply-to-folder) use findRunningScanJob/startScanJob directly instead, since
// they need the account in the lookup too.
func (s *Store) handleOwnerJobSSE(
	w http.ResponseWriter, r *http.Request, owner, kind string,
	run func(ctx context.Context, onProgress func(done, total int)) (any, error),
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var sendMu sync.Mutex
	sendEvent := func(event string, data any) {
		sendMu.Lock()
		defer sendMu.Unlock()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	jobID, _, alreadyRunning := s.findRunningScanJobForOwner(owner, kind)
	if !alreadyRunning {
		var err error
		jobID, err = s.startScanJob(owner, "", kind)
		if err != nil {
			sendEvent("error", map[string]string{"message": err.Error()})
			return
		}
		go s.runScanJob(jobID, kind, run)
	}
	sendEvent("job", map[string]string{"jobId": jobID})

	err := s.subscribeScanJob(r.Context(), jobID, func(snap scanJobSnapshot) {
		switch snap.Status {
		case "running":
			sendEvent("progress", map[string]int{"done": snap.Done, "total": snap.Total})
		case "done":
			sendEvent("complete", snap.Summary)
		case "error":
			sendEvent("error", map[string]string{"message": snap.Error})
		default: // cancelled, interrupted
			sendEvent("cancelled", map[string]string{})
		}
	})
	if err != nil && r.Context().Err() == nil {
		sendEvent("error", map[string]string{"message": err.Error()})
	}
}

// subscribeScanJob polls a job's row until it stops running, calling onUpdate on every
// change (at least once, even for a job that's already finished by the time this is
// called — a client reconnecting after a job completed still needs to see that). ctx
// ending (the SSE connection closing) just stops this one subscriber; it does not
// touch the job itself.
func (s *Store) subscribeScanJob(ctx context.Context, jobID string, onUpdate func(scanJobSnapshot)) error {
	var last scanJobSnapshot
	first := true
	for {
		snap, err := s.scanJobSnapshotByID(jobID)
		if err != nil {
			return err
		}
		changed := first || snap.Status != last.Status || snap.Done != last.Done || snap.Total != last.Total
		// A freshly-created job's row starts at done=0/total=0, before the scan itself
		// has actually counted anything — sending that as the very first update flashed
		// "Scanned 0 of 0" for a moment before the real count replaced it. Skipped only
		// on this exact first tick; a genuine 0/0 later (an empty account) still gets
		// reported once it's real.
		skipBlankFirst := first && snap.Status == "running" && snap.Done == 0 && snap.Total == 0
		if changed && !skipBlankFirst {
			onUpdate(snap)
		}
		last = snap
		first = false
		if snap.Status != "running" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scanJobPollInterval):
		}
	}
}
