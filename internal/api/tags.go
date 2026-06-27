package api

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// TagsRoutes registers tag CRUD, per-mail tag assignment, and folder->tag auto-rules.
func (s *Store) TagsRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		s.handleListTags(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/tags", func(w http.ResponseWriter, r *http.Request) {
		s.handleCreateTag(w, r, ownerSubject(r))
	})
	mux.HandleFunc("PATCH /api/tags/{id}", s.handleUpdateTag)
	mux.HandleFunc("DELETE /api/tags/{id}", s.handleDeleteTag)

	mux.HandleFunc("GET /api/mails/{id}/tags", s.handleGetMailTags)
	mux.HandleFunc("PUT /api/mails/{id}/tags", s.handleSetMailTags)

	mux.HandleFunc("GET /api/accounts/{id}/folder-tag-rules", s.handleListFolderTagRules)
	mux.HandleFunc("PUT /api/accounts/{id}/folder-tag-rules", s.handleSetFolderTagRule)
	mux.HandleFunc("DELETE /api/accounts/{id}/folder-tag-rules", s.handleDeleteFolderTagRule)

	// The same folder_tag_rules table, but assigned tag-first instead of folder-first —
	// "this tag goes to that folder" rather than "mail moved into this folder gets that
	// tag". Lets a tag be given a destination without first navigating to the folder
	// itself, and is what autoMoveTaggedMail (smarttags.go) reads to know where a
	// tagged mail belongs once it's old enough to move automatically.
	mux.HandleFunc("GET /api/tag-destinations", func(w http.ResponseWriter, r *http.Request) {
		s.handleListTagDestinations(w, r, ownerSubject(r))
	})
	mux.HandleFunc("PUT /api/accounts/{id}/tag-destination", s.handleSetTagDestination)
	mux.HandleFunc("DELETE /api/accounts/{id}/tag-destination", s.handleDeleteTagDestination)

	// Trusting a sender lets remote images in their HTML mail load automatically going
	// forward — see handleMailBody (mails.go) for the default-blocked side of this.
	mux.HandleFunc("POST /api/trusted-senders", func(w http.ResponseWriter, r *http.Request) {
		s.handleTrustSender(w, r, ownerSubject(r))
	})
}

func (s *Store) handleTrustSender(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		SenderEmail string `json:"senderEmail"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SenderEmail == "" {
		http.Error(w, "senderEmail is required", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec(
		"INSERT INTO trusted_senders (owner_subject, sender_email) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		owner, req.SenderEmail,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// A handful of pleasant, distinct colors, cycled deterministically by name so the
// same tag name always lands on the same color without the user having to pick one.
var tagPalette = []string{"#ff3d81", "#00d9a3", "#9b5de5", "#ff9f1c", "#3a86ff", "#f72585", "#06d6a0"}

func colorForName(name string) string {
	sum := sha256.Sum256([]byte(name))
	i := binary.BigEndian.Uint32(sum[:4]) % uint32(len(tagPalette))
	return tagPalette[i]
}

func (s *Store) handleListTags(w http.ResponseWriter, r *http.Request, owner string) {
	rows, err := s.db.Query("SELECT id, name, color, notify, instant_move FROM tags WHERE owner_subject = $1 ORDER BY name", owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	tags := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Notify, &t.InstantMove); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tags = append(tags, t)
	}
	writeJSON(w, tags)
}

func (s *Store) handleCreateTag(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	id := randomID()
	color := colorForName(name)
	row := s.db.QueryRow(`
		INSERT INTO tags (id, owner_subject, name, color) VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_subject, name) DO UPDATE SET name = excluded.name
		RETURNING id, name, color, notify, instant_move`,
		id, owner, name, color,
	)
	var t Tag
	if err := row.Scan(&t.ID, &t.Name, &t.Color, &t.Notify, &t.InstantMove); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

// handleUpdateTag renames and/or recolors a tag in place — both fields optional, only
// the ones present in the body get changed.
func (s *Store) handleUpdateTag(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name        *string `json:"name"`
		Color       *string `json:"color"`
		Notify      *bool   `json:"notify"`
		InstantMove *bool   `json:"instantMove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if _, err := s.db.Exec("UPDATE tags SET name = $1 WHERE id = $2", name, id); err != nil {
			http.Error(w, "that name is already taken", http.StatusBadRequest)
			return
		}
	}
	if req.Color != nil && *req.Color != "" {
		if _, err := s.db.Exec("UPDATE tags SET color = $1 WHERE id = $2", *req.Color, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.Notify != nil {
		if _, err := s.db.Exec("UPDATE tags SET notify = $1 WHERE id = $2", *req.Notify, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.InstantMove != nil {
		if _, err := s.db.Exec("UPDATE tags SET instant_move = $1 WHERE id = $2", *req.InstantMove, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	var t Tag
	if err := s.db.QueryRow("SELECT id, name, color, notify, instant_move FROM tags WHERE id = $1", id).Scan(&t.ID, &t.Name, &t.Color, &t.Notify, &t.InstantMove); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, t)
}

func (s *Store) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// The Spam tag is load-bearing for the whole spam detection system (image
	// gating, folder_tag_rules, the Smart Spam panel's queries all key off a tag
	// literally named "Spam") — deleting it doesn't just lose a label, it silently
	// breaks that entire feature. Rename it if "Spam" isn't the wording you want.
	var name string
	if err := s.db.QueryRow("SELECT name FROM tags WHERE id = $1", id).Scan(&name); err == nil && name == "Spam" {
		http.Error(w, "the Spam tag can't be deleted", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec("DELETE FROM tags WHERE id = $1", id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleGetMailTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	messageID, err := s.resolveMessageID(id)
	if err != nil {
		// no message id available even after trying to fetch one (e.g. mock mail) —
		// that just means no tags, not an error the UI needs to see
		writeJSON(w, []Tag{})
		return
	}
	writeJSON(w, s.tagsForMessage(messageID))
}

// resolveMessageID returns a mail's Message-ID, fetching it live over IMAP and
// persisting it if the cached row predates message_id being tracked at all (an older
// sync, from before tags existed) — rather than just failing tag operations on any
// mail that hasn't happened to resync since.
func (s *Store) resolveMessageID(mailID string) (string, error) {
	var accountID *string
	var uid *int64
	var folder, messageID string
	err := s.db.QueryRow(
		"SELECT account_id, uid, folder, coalesce(message_id, '') FROM mails WHERE id = $1", mailID,
	).Scan(&accountID, &uid, &folder, &messageID)
	if err != nil {
		return "", err
	}
	if messageID != "" {
		return messageID, nil
	}
	if accountID == nil || uid == nil {
		return "", fmt.Errorf("mock mail has no message id")
	}

	acct, password, err := s.loadAccountCreds(*accountID)
	if err != nil {
		return "", err
	}
	messageID, err = fetchMessageID(acct, password, folder, uint32(*uid))
	if err != nil {
		return "", err
	}
	s.db.Exec("UPDATE mails SET message_id = $1 WHERE id = $2", messageID, mailID)
	return messageID, nil
}

func (s *Store) tagsForMessage(messageID string) []Tag {
	tags := []Tag{}
	if messageID == "" {
		return tags
	}
	rows, err := s.db.Query(`
		SELECT t.id, t.name, t.color FROM message_tags mt JOIN tags t ON t.id = mt.tag_id
		WHERE mt.message_id = $1 ORDER BY t.name`, messageID)
	if err != nil {
		return tags
	}
	defer rows.Close()
	for rows.Next() {
		var t Tag
		if rows.Scan(&t.ID, &t.Name, &t.Color) == nil {
			tags = append(tags, t)
		}
	}
	return tags
}

// handleSetMailTags replaces the full tag set for a mail (simpler for the UI than
// separate add/remove calls — it just sends the checked set each time).
func (s *Store) handleSetMailTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		TagIDs []string `json:"tagIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	messageID, err := s.resolveMessageID(id)
	if err != nil {
		http.Error(w, "this mail has no message id to tag ("+err.Error()+")", http.StatusBadRequest)
		return
	}

	before := s.tagsForMessage(messageID)
	if err := s.setMessageTags(messageID, req.TagIDs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.recordManualTagChange(messageID, before, req.TagIDs)
	writeJSON(w, s.tagsForMessage(messageID))
}

func (s *Store) setMessageTags(messageID string, tagIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM message_tags WHERE message_id = $1", messageID); err != nil {
		return err
	}
	for _, tagID := range tagIDs {
		if _, err := tx.Exec(
			"INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			messageID, tagID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// recordManualTagChange diffs the tag set before/after a manual edit and logs to
// tag_history: net-new additions as a manual 'applied' decision (skipping tags that
// were already set — toggling the same tag on/off repeatedly shouldn't spam duplicate
// rows), and removals of a tag that smart-tagging previously applied as a 'dismissed'
// decision — without this, removing a wrong auto-applied tag teaches the suggester
// nothing, and it would just reapply the same tag next time. Best-effort: a mail with
// no account (mock mail) or no owner has nothing to log against, and that's fine.
func (s *Store) recordManualTagChange(messageID string, before []Tag, afterIDs []string) {
	var accountID, senderEmail, subject string
	if err := s.db.QueryRow(
		"SELECT coalesce(account_id, ''), coalesce(sender_email, ''), subject FROM mails WHERE message_id = $1 LIMIT 1",
		messageID,
	).Scan(&accountID, &senderEmail, &subject); err != nil || accountID == "" {
		return
	}
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		return
	}

	beforeIDs := map[string]bool{}
	for _, t := range before {
		beforeIDs[t.ID] = true
	}
	afterSet := map[string]bool{}
	for _, id := range afterIDs {
		afterSet[id] = true
	}

	// Best-effort lookup, not a hard requirement — "" just means the tagID == spamTagID
	// check below never matches, same as if this account had never touched spam at all.
	spamTagID, _ := s.getOrCreateSpamTag(ownerSubject)

	addedAny := false
	for _, tagID := range afterIDs {
		if !beforeIDs[tagID] {
			s.recordTagHistory(ownerSubject, accountID, messageID, tagID, senderEmail, subject, "manual", "applied", nil, nil)
			addedAny = true
			if tagID == spamTagID {
				// Manually marking something Spam should move it to Junk the same way
				// the spam engine's own auto-apply already does — ensureSpamFolderRule
				// otherwise only ever runs from the scoring paths (prefetchMailImages,
				// scanAccountForSpam), so a manual tag with no prior scan/live-score on
				// this account had no folder rule yet for autoMoveTaggedMail (below) to
				// actually use, and silently moved nothing.
				s.ensureSpamFolderRule(accountID, tagID)
			}
		}
	}
	if addedAny {
		// Same immediate check as accepting a suggestion does — still goes through the
		// normal delay+inbox rule, not around it: only moves now if this mail/tag
		// combination already qualifies (e.g. an older tag_history row), otherwise it
		// just waits for the delay like normal.
		s.autoMoveTaggedMail(ownerSubject)
	}
	for tagID := range beforeIDs {
		if afterSet[tagID] {
			continue
		}
		// Always recorded, not just when smart-tagging already had prior history for
		// this sender+tag — a manual removal (including the misplaced-mail panel's
		// "Remove tag", which goes through this same path) is an explicit verdict
		// either way. Skipping the record here used to mean nothing ever told
		// suppressedTags this exact message shouldn't get this tag again, so a later
		// scan or sync was free to silently reapply the very tag just removed.
		s.recordTagHistory(ownerSubject, accountID, messageID, tagID, senderEmail, subject, "manual", "dismissed", nil, nil)
	}
}

func (s *Store) handleListFolderTagRules(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	rows, err := s.db.Query(`
		SELECT ftr.folder, t.id, t.name, t.color FROM folder_tag_rules ftr
		JOIN tags t ON t.id = ftr.tag_id WHERE ftr.account_id = $1`, accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type rule struct {
		Folder string `json:"folder"`
		Tag    Tag    `json:"tag"`
	}
	rules := []rule{}
	for rows.Next() {
		var rl rule
		if err := rows.Scan(&rl.Folder, &rl.Tag.ID, &rl.Tag.Name, &rl.Tag.Color); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rules = append(rules, rl)
	}
	writeJSON(w, rules)
}

// handleSetFolderTagRule is handleSetTagDestination's folder-first sibling (the folder
// banner's own "auto-tag" dropdown, rather than the Tag Manager) — same row, same
// table, just reached from the other side. It used to skip the "clear this tag's other
// folder first" step that handleSetTagDestination already does, which let a tag end up
// pointed at two folders at once: autoMoveTaggedMail refuses to guess when that happens
// (more than one destination is ambiguous, so it just silently moves nothing for that
// tag), and the Tag Manager's "remove" action deletes a tag's destination by tag_id,
// not by folder — so clearing what looked like "the second one" actually cleared both,
// since from that action's own perspective a tag only ever has one destination to clear.
func (s *Store) handleSetFolderTagRule(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	var req struct {
		Folder string `json:"folder"`
		TagID  string `json:"tagId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Folder == "" || req.TagID == "" {
		http.Error(w, "folder and tagId are required", http.StatusBadRequest)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"DELETE FROM folder_tag_rules WHERE account_id = $1 AND tag_id = $2 AND folder != $3",
		accountID, req.TagID, req.Folder,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(`
		INSERT INTO folder_tag_rules (account_id, folder, tag_id) VALUES ($1, $2, $3)
		ON CONFLICT (account_id, folder) DO UPDATE SET tag_id = excluded.tag_id`,
		accountID, req.Folder, req.TagID,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleDeleteFolderTagRule(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		http.Error(w, "folder is required", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec("DELETE FROM folder_tag_rules WHERE account_id = $1 AND folder = $2", accountID, folder); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListTagDestinations lists every tag->folder assignment across every account
// the owner has — pooled the same way tag scoring already is (owner_subject, not a
// single account_id), so the Tags settings panel can show "this tag goes to X" without
// the caller needing to already know which account that folder lives on.
func (s *Store) handleListTagDestinations(w http.ResponseWriter, r *http.Request, owner string) {
	rows, err := s.db.Query(`
		SELECT ftr.account_id, a.email, ftr.folder, ftr.tag_id
		FROM folder_tag_rules ftr
		JOIN accounts a ON a.id = ftr.account_id
		WHERE a.owner_subject = $1`, owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type destination struct {
		AccountID    string `json:"accountId"`
		AccountEmail string `json:"accountEmail"`
		Folder       string `json:"folder"`
		TagID        string `json:"tagId"`
	}
	destinations := []destination{}
	for rows.Next() {
		var d destination
		if err := rows.Scan(&d.AccountID, &d.AccountEmail, &d.Folder, &d.TagID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		destinations = append(destinations, d)
	}
	writeJSON(w, destinations)
}

// handleSetTagDestination assigns a tag to a folder it should move to — the same
// folder_tag_rules row the folder banner's "auto-tag" dropdown writes, just reached
// from the tag's side instead of the folder's. Clears any other folder this tag was
// previously pointed to on this account first: autoMoveTaggedMail deliberately
// refuses to guess when a tag maps to more than one folder, so this keeps that always
// true going forward rather than letting stale assignments accumulate.
func (s *Store) handleSetTagDestination(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	var req struct {
		Folder string `json:"folder"`
		TagID  string `json:"tagId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Folder == "" || req.TagID == "" {
		http.Error(w, "folder and tagId are required", http.StatusBadRequest)
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		"DELETE FROM folder_tag_rules WHERE account_id = $1 AND tag_id = $2 AND folder != $3",
		accountID, req.TagID, req.Folder,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(`
		INSERT INTO folder_tag_rules (account_id, folder, tag_id) VALUES ($1, $2, $3)
		ON CONFLICT (account_id, folder) DO UPDATE SET tag_id = excluded.tag_id`,
		accountID, req.Folder, req.TagID,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleDeleteTagDestination(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	tagID := r.URL.Query().Get("tagId")
	if tagID == "" {
		http.Error(w, "tagId is required", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec("DELETE FROM folder_tag_rules WHERE account_id = $1 AND tag_id = $2", accountID, tagID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyFolderTagRule auto-tags a message if its destination folder has a rule —
// called right before/after a move completes. Best-effort: a failure here shouldn't
// fail the move itself.
func (s *Store) applyFolderTagRule(accountID, destFolder, messageID, source string) {
	if messageID == "" {
		return
	}
	var tagID string
	err := s.db.QueryRow(
		"SELECT tag_id FROM folder_tag_rules WHERE account_id = $1 AND folder = $2", accountID, destFolder,
	).Scan(&tagID)
	if err != nil {
		return // no rule for this folder — the common case, not an error
	}
	res, err := s.db.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", messageID, tagID)
	if err != nil {
		return
	}
	// The ON CONFLICT above makes this idempotent for the tag assignment itself, but
	// recordTagHistory below isn't — it always inserts a fresh row. Something
	// upstream (autoMoveTaggedMail re-selecting a message that never actually leaves
	// the inbox) can call this repeatedly for mail that's already tagged; without this
	// check each repeat logged an identical history row, quietly inflating that
	// sender/tag's applied count and skewing senderRatio/domainRatio scoring.
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return
	}

	// Best-effort history logging — a lookup failure here shouldn't undo the move/tag
	// that already happened, it just means this particular application doesn't feed
	// the smart-tagging scorer.
	var senderEmail, subject string
	if err := s.db.QueryRow(
		"SELECT coalesce(sender_email, ''), subject FROM mails WHERE message_id = $1 LIMIT 1", messageID,
	).Scan(&senderEmail, &subject); err != nil {
		return
	}
	var ownerSubject string
	if err := s.db.QueryRow("SELECT owner_subject FROM accounts WHERE id = $1", accountID).Scan(&ownerSubject); err != nil {
		return
	}
	s.recordTagHistory(ownerSubject, accountID, messageID, tagID, senderEmail, subject, source, "applied", nil, nil)
}

// filterByTags keeps only the mails tagged with at least one of tagIDs (an "any of"
// filter, matching how the account filter chips behave).
func (s *Store) filterByTags(mails []Mail, tagIDs []string) []Mail {
	if len(mails) == 0 {
		return mails
	}
	messageIDs := make([]string, 0, len(mails))
	for _, m := range mails {
		if m.MessageID != "" {
			messageIDs = append(messageIDs, m.MessageID)
		}
	}
	if len(messageIDs) == 0 {
		return nil
	}

	rows, err := s.db.Query(
		"SELECT DISTINCT message_id FROM message_tags WHERE message_id = ANY($1) AND tag_id = ANY($2)",
		messageIDs, tagIDs,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	tagged := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			tagged[id] = true
		}
	}

	kept := make([]Mail, 0, len(mails))
	for _, m := range mails {
		if tagged[m.MessageID] {
			kept = append(kept, m)
		}
	}
	return kept
}
