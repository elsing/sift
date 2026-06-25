package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// backupKDFIterations follows OWASP's 2023 minimum for PBKDF2-HMAC-SHA256 — high
// enough to make brute-forcing a stolen backup file expensive, cheap enough that
// deriving the key once per export/import is unnoticeable.
const backupKDFIterations = 600_000

// encryptedBackup is what gets written to the file when an export password is
// given — the inner exportBundle JSON, AES-GCM-encrypted with a key derived from
// that password. Lets the backup itself be opaque even though the bundle contains
// plaintext IMAP/OAuth credentials (decrypted from the DB for portability to a
// fresh deployment's ENCRYPTION_KEY).
type encryptedBackup struct {
	Encrypted  bool   `json:"encrypted"`
	Salt       string `json:"salt"`
	Ciphertext string `json:"ciphertext"`
}

func encryptBackup(password string, plaintext []byte) (encryptedBackup, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return encryptedBackup{}, err
	}
	gcm, err := backupGCM(password, salt)
	if err != nil {
		return encryptedBackup{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return encryptedBackup{}, err
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return encryptedBackup{
		Encrypted:  true,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

func decryptBackup(password string, eb encryptedBackup) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(eb.Salt)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(eb.Ciphertext)
	if err != nil {
		return nil, err
	}
	gcm, err := backupGCM(password, salt)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, data := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, data, nil) // wrong password surfaces here as an auth failure, not garbage output
}

func backupGCM(password string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, backupKDFIterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// backupInclude is which top-level categories an export should query/include, or an
// import should apply from the file — same four categories either direction, since
// "what gets exported" and "what gets overwritten on import" are the same question
// asked at the other end. Missing/absent means "all four", so an older client (or a
// plain curl call) gets today's full-bundle behavior without having to know this
// exists.
type backupInclude struct{ accounts, tags, history, settings bool }

func parseBackupInclude(m map[string]bool) backupInclude {
	if len(m) == 0 {
		return backupInclude{true, true, true, true}
	}
	return backupInclude{accounts: m["accounts"], tags: m["tags"], history: m["history"], settings: m["settings"]}
}

// DataTransferRoutes registers the full backup/restore endpoints: everything needed
// to stand up a copy-paste deployment from scratch — connected IMAP/OAuth accounts
// and their folder selection, tag definitions, per-mail tag assignments, folder->tag
// rules, trusted senders, sender tag-blocks, the smart-tagging learned history
// (tag_history — the actual applied/dismissed data senderRatio/domainRatio/
// subjectRatio score off, not just a log) and spam_flags diagnostic history, the
// owner's auto-tag/spam/image-cache settings, and (passed through opaquely, see
// LocalPreferences) the browser-only personalisation settings — theme, palette,
// swipe actions, fun-delete-animation, auto-load-images — that never touch the
// server otherwise.
// Deliberately excludes push subscriptions (browser-specific, can't be replayed on
// another device) and the cached archive/trash/junk folder names (auto-detected
// fresh on first use).
// POST, not GET, because the client has to send its localStorage preferences for the
// server to fold into the bundle before it's (optionally) encrypted.
func (s *Store) DataTransferRoutes(mux *http.ServeMux, ownerSubject func(*http.Request) string) {
	mux.HandleFunc("POST /api/backup/export", func(w http.ResponseWriter, r *http.Request) {
		s.handleExportData(w, r, ownerSubject(r))
	})
	mux.HandleFunc("POST /api/backup/import", func(w http.ResponseWriter, r *http.Request) {
		s.handleImportData(w, r, ownerSubject(r))
	})
}

type backupAccount struct {
	Email             string   `json:"email"`
	Host              string   `json:"host"`
	Port              int      `json:"port"`
	Username          string   `json:"username"`
	Password          string   `json:"password,omitempty"` // plaintext — decrypted on export, re-encrypted on import. Sensitive: this file is a full credentials dump.
	OAuthProvider     string   `json:"oauthProvider,omitempty"`
	OAuthRefreshToken string   `json:"oauthRefreshToken,omitempty"` // plaintext, same caveat as Password
	ExpandedFolders   []string `json:"expandedFolders"`
}

type exportBundle struct {
	Accounts []backupAccount `json:"accounts"`
	Tags     []struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Notify      bool   `json:"notify"`
		InstantMove bool   `json:"instantMove"`
	} `json:"tags"`
	MessageTags []struct {
		MessageID string `json:"messageId"`
		TagName   string `json:"tagName"`
	} `json:"messageTags"`
	FolderTagRules []struct {
		AccountEmail string `json:"accountEmail"`
		Folder       string `json:"folder"`
		TagName      string `json:"tagName"`
	} `json:"folderTagRules"`
	TrustedSenders  []string `json:"trustedSenders"`
	TagSenderBlocks []struct {
		SenderEmail string `json:"senderEmail"`
		TagName     string `json:"tagName"`
	} `json:"tagSenderBlocks"`
	// The actual learned state behind smart tagging — every past applied/dismissed
	// decision, which is what senderRatio/domainRatio/subjectRatio (smarttags.go) score
	// off. Without this, a restored install starts smart tagging completely cold even
	// though every tag/rule it depends on came back.
	TagHistory []struct {
		MessageID     string   `json:"messageId"`
		TagName       string   `json:"tagName"`
		SenderEmail   string   `json:"senderEmail"`
		SenderDomain  string   `json:"senderDomain"`
		SubjectTokens []string `json:"subjectTokens"`
		Source        string   `json:"source"`
		Status        string   `json:"status"`
		Score         *float64 `json:"score,omitempty"`
		CreatedAt     string   `json:"createdAt"`
	} `json:"tagHistory"`
	// Past spam scoring results — diagnostic (the reader's "why flagged" readout), not
	// an input to scoreSpam itself, but still real user-facing history worth restoring.
	SpamFlags []struct {
		MessageID string  `json:"messageId"`
		Score     float64 `json:"score"`
		Reasons   string  `json:"reasons"`
		SPF       string  `json:"spf"`
		DKIM      string  `json:"dkim"`
		DMARC     string  `json:"dmarc"`
		CreatedAt string  `json:"createdAt"`
	} `json:"spamFlags"`
	OwnerSettings struct {
		AutoTagMode             string `json:"autoTagMode"`
		SpamMode                string `json:"spamMode"`
		AutoMoveDelayDays       int    `json:"autoMoveDelayDays"`
		ImageCacheRetentionDays int    `json:"imageCacheRetentionDays"`
	} `json:"ownerSettings"`
	// Opaque key/value pass-through for browser-only settings (theme, palette, swipe
	// actions, etc.) — the server never reads or writes these, just carries them from
	// export to import so the client can restore them into localStorage itself.
	LocalPreferences map[string]string `json:"localPreferences,omitempty"`
}

func (s *Store) handleExportData(w http.ResponseWriter, r *http.Request, owner string) {
	var b exportBundle
	var req struct {
		LocalPreferences map[string]string `json:"localPreferences"`
		Include          map[string]bool   `json:"include"`
	}
	json.NewDecoder(r.Body).Decode(&req) // best-effort — an empty/missing body just means no local prefs/include filter
	include := parseBackupInclude(req.Include)
	if include.settings {
		b.LocalPreferences = req.LocalPreferences
	}

	if include.accounts {
		rows, err := s.db.Query(`
			SELECT id, email, host, port, username, password_enc, oauth_provider, oauth_refresh_token_enc, expanded_folders
			FROM accounts WHERE owner_subject = $1`, owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var id string
			var encPassword, encRefresh []byte
			var expandedJSON string
			var a backupAccount
			if rows.Scan(&id, &a.Email, &a.Host, &a.Port, &a.Username, &encPassword, &a.OAuthProvider, &encRefresh, &expandedJSON) != nil {
				continue
			}
			json.Unmarshal([]byte(expandedJSON), &a.ExpandedFolders)
			if a.OAuthProvider == "" {
				if pw, err := s.crypto.decrypt(encPassword); err == nil {
					a.Password = pw
				}
			} else if rt, err := s.crypto.decrypt(encRefresh); err == nil {
				a.OAuthRefreshToken = rt
			}
			b.Accounts = append(b.Accounts, a)
		}
		rows.Close()
	}

	if include.tags {
		rows, err := s.db.Query("SELECT name, color, notify, instant_move FROM tags WHERE owner_subject = $1", owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var t struct {
				Name        string
				Color       string
				Notify      bool
				InstantMove bool
			}
			if rows.Scan(&t.Name, &t.Color, &t.Notify, &t.InstantMove) == nil {
				b.Tags = append(b.Tags, struct {
					Name        string `json:"name"`
					Color       string `json:"color"`
					Notify      bool   `json:"notify"`
					InstantMove bool   `json:"instantMove"`
				}{t.Name, t.Color, t.Notify, t.InstantMove})
			}
		}
		rows.Close()

		rows, err = s.db.Query(`
			SELECT mt.message_id, t.name FROM message_tags mt
			JOIN tags t ON t.id = mt.tag_id WHERE t.owner_subject = $1`, owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var messageID, tagName string
			if rows.Scan(&messageID, &tagName) == nil {
				b.MessageTags = append(b.MessageTags, struct {
					MessageID string `json:"messageId"`
					TagName   string `json:"tagName"`
				}{messageID, tagName})
			}
		}
		rows.Close()

		rows, err = s.db.Query(`
			SELECT a.email, ftr.folder, t.name FROM folder_tag_rules ftr
			JOIN accounts a ON a.id = ftr.account_id
			JOIN tags t ON t.id = ftr.tag_id WHERE a.owner_subject = $1`, owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var email, folder, tagName string
			if rows.Scan(&email, &folder, &tagName) == nil {
				b.FolderTagRules = append(b.FolderTagRules, struct {
					AccountEmail string `json:"accountEmail"`
					Folder       string `json:"folder"`
					TagName      string `json:"tagName"`
				}{email, folder, tagName})
			}
		}
		rows.Close()

		rows, err = s.db.Query("SELECT sender_email FROM trusted_senders WHERE owner_subject = $1", owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var email string
			if rows.Scan(&email) == nil {
				b.TrustedSenders = append(b.TrustedSenders, email)
			}
		}
		rows.Close()

		rows, err = s.db.Query(`
			SELECT tsb.sender_email, t.name FROM tag_sender_blocks tsb
			JOIN tags t ON t.id = tsb.tag_id WHERE tsb.owner_subject = $1`, owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var email, tagName string
			if rows.Scan(&email, &tagName) == nil {
				b.TagSenderBlocks = append(b.TagSenderBlocks, struct {
					SenderEmail string `json:"senderEmail"`
					TagName     string `json:"tagName"`
				}{email, tagName})
			}
		}
		rows.Close()
	}

	if include.history {
		// subject_tokens is TEXT[] — pgx's stdlib database/sql driver can't Scan a
		// postgres array column directly into a Go []string (same issue migration 0030
		// hit for spam_flags.reasons), so it's pulled out as newline-joined text instead
		// and split back apart here. Tokens are always plain lowercased words
		// (tokenizeSubject strips punctuation), so a literal newline can't appear in one.
		rows, err := s.db.Query(`
			SELECT th.message_id, t.name, th.sender_email, th.sender_domain, array_to_string(th.subject_tokens, E'\n'), th.source, th.status, th.score, th.created_at
			FROM tag_history th JOIN tags t ON t.id = th.tag_id WHERE th.owner_subject = $1`, owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var h struct {
				MessageID, TagName, SenderEmail, SenderDomain, Source, Status, SubjectTokens string
				Score                                                                        *float64
				CreatedAt                                                                    time.Time
			}
			if rows.Scan(&h.MessageID, &h.TagName, &h.SenderEmail, &h.SenderDomain, &h.SubjectTokens, &h.Source, &h.Status, &h.Score, &h.CreatedAt) == nil {
				var tokens []string
				if h.SubjectTokens != "" {
					tokens = strings.Split(h.SubjectTokens, "\n")
				}
				b.TagHistory = append(b.TagHistory, struct {
					MessageID     string   `json:"messageId"`
					TagName       string   `json:"tagName"`
					SenderEmail   string   `json:"senderEmail"`
					SenderDomain  string   `json:"senderDomain"`
					SubjectTokens []string `json:"subjectTokens"`
					Source        string   `json:"source"`
					Status        string   `json:"status"`
					Score         *float64 `json:"score,omitempty"`
					CreatedAt     string   `json:"createdAt"`
				}{h.MessageID, h.TagName, h.SenderEmail, h.SenderDomain, tokens, h.Source, h.Status, h.Score, h.CreatedAt.Format(time.RFC3339)})
			}
		}
		rows.Close()

		spamRows, err := s.db.Query(
			"SELECT message_id, score, reasons, spf, dkim, dmarc, created_at FROM spam_flags WHERE owner_subject = $1", owner)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for spamRows.Next() {
			var f struct {
				MessageID, Reasons, SPF, DKIM, DMARC string
				Score                                float64
				CreatedAt                            time.Time
			}
			if spamRows.Scan(&f.MessageID, &f.Score, &f.Reasons, &f.SPF, &f.DKIM, &f.DMARC, &f.CreatedAt) == nil {
				b.SpamFlags = append(b.SpamFlags, struct {
					MessageID string  `json:"messageId"`
					Score     float64 `json:"score"`
					Reasons   string  `json:"reasons"`
					SPF       string  `json:"spf"`
					DKIM      string  `json:"dkim"`
					DMARC     string  `json:"dmarc"`
					CreatedAt string  `json:"createdAt"`
				}{f.MessageID, f.Score, f.Reasons, f.SPF, f.DKIM, f.DMARC, f.CreatedAt.Format(time.RFC3339)})
			}
		}
		spamRows.Close()
	}

	if include.settings {
		b.OwnerSettings.AutoTagMode = "review"
		b.OwnerSettings.SpamMode = "review"
		b.OwnerSettings.AutoMoveDelayDays = 3
		b.OwnerSettings.ImageCacheRetentionDays = 90
		s.db.QueryRow(
			"SELECT auto_tag_mode, spam_mode, auto_move_delay_days, image_cache_retention_days FROM owner_settings WHERE owner_subject = $1", owner,
		).Scan(&b.OwnerSettings.AutoTagMode, &b.OwnerSettings.SpamMode, &b.OwnerSettings.AutoMoveDelayDays, &b.OwnerSettings.ImageCacheRetentionDays)
	}

	w.Header().Set("Content-Disposition", `attachment; filename="sift-backup.json"`)
	if password := r.Header.Get("X-Backup-Password"); password != "" {
		plaintext, err := json.Marshal(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		eb, err := encryptBackup(password, plaintext)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, eb)
		return
	}
	writeJSON(w, b)
}

// handleImportData restores a backup: connects/updates accounts first (matched by
// email — account ids aren't portable across installs), then upserts tags by name,
// then everything that hangs off tags/accounts. Best-effort throughout: one bad row
// (an account whose password no longer works, a stale tag reference) shouldn't sink
// the rest of the restore.
func (s *Store) handleImportData(w http.ResponseWriter, r *http.Request, owner string) {
	include := backupInclude{true, true, true, true}
	if raw := r.Header.Get("X-Backup-Include"); raw != "" {
		m := map[string]bool{}
		for _, cat := range strings.Split(raw, ",") {
			m[strings.TrimSpace(cat)] = true
		}
		include = parseBackupInclude(m)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	var eb encryptedBackup
	if json.Unmarshal(body, &eb) == nil && eb.Encrypted {
		password := r.Header.Get("X-Backup-Password")
		if password == "" {
			http.Error(w, "this backup is password-protected — provide the password", http.StatusBadRequest)
			return
		}
		plaintext, err := decryptBackup(password, eb)
		if err != nil {
			http.Error(w, "wrong password, or the backup file is corrupt", http.StatusBadRequest)
			return
		}
		body = plaintext
	}

	var b exportBundle
	if err := json.Unmarshal(body, &b); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	accountIDByEmail := map[string]string{}
	rows, err := s.db.Query("SELECT id, email FROM accounts WHERE owner_subject = $1", owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var id, email string
		if rows.Scan(&id, &email) == nil {
			accountIDByEmail[email] = id
		}
	}
	rows.Close()

	if include.accounts {
		for _, a := range b.Accounts {
			encoded, _ := json.Marshal(a.ExpandedFolders)
			if id, ok := accountIDByEmail[a.Email]; ok {
				s.db.Exec("UPDATE accounts SET expanded_folders = $1 WHERE id = $2", string(encoded), id)
				continue
			}

			id := randomID()
			if a.OAuthProvider == "" {
				if err := testIMAPLogin(a.Host, a.Port, a.Username, a.Password); err != nil {
					log.Printf("backup import %s: imap login failed, skipping: %v", a.Email, err)
					continue
				}
				encPassword, err := s.crypto.encrypt(a.Password)
				if err != nil {
					continue
				}
				if _, err := s.db.Exec(
					"INSERT INTO accounts (id, owner_subject, email, host, port, username, password_enc, expanded_folders) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
					id, owner, a.Email, a.Host, a.Port, a.Username, encPassword, string(encoded),
				); err != nil {
					log.Printf("backup import %s: insert: %v", a.Email, err)
					continue
				}
			} else {
				encRefresh, err := s.crypto.encrypt(a.OAuthRefreshToken)
				if err != nil {
					continue
				}
				if _, err := s.db.Exec(
					`INSERT INTO accounts (id, owner_subject, email, host, port, username, oauth_provider, oauth_refresh_token_enc, expanded_folders)
					 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
					id, owner, a.Email, a.Host, a.Port, a.Username, a.OAuthProvider, encRefresh, string(encoded),
				); err != nil {
					log.Printf("backup import %s: insert: %v", a.Email, err)
					continue
				}
			}
			accountIDByEmail[a.Email] = id
			if _, err := s.syncAccount(id); err != nil {
				log.Printf("backup import %s: initial sync failed: %v", a.Email, err)
			}
			s.cleanupMockMail()
			s.watchAccount(id)
		}
	}

	// Everything from here down is potentially thousands of rows (tag_history alone
	// can be five figures) — issued one at a time outside a transaction, each INSERT
	// was its own autocommit round trip (a real fsync per statement), which made a
	// real-sized import look hung for the better part of a minute with zero feedback.
	// One transaction defers all of that to a single commit at the end.
	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	tagIDByName := map[string]string{}
	rows, err = tx.Query("SELECT id, name FROM tags WHERE owner_subject = $1", owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			tagIDByName[name] = id
		}
	}
	rows.Close()

	if include.tags {
		for _, t := range b.Tags {
			if id, ok := tagIDByName[t.Name]; ok {
				tx.Exec("UPDATE tags SET color = $1, notify = $2, instant_move = $3 WHERE id = $4",
					t.Color, t.Notify, t.InstantMove, id)
				continue
			}
			id := randomID()
			if _, err := tx.Exec(
				"INSERT INTO tags (id, owner_subject, name, color, notify, instant_move) VALUES ($1, $2, $3, $4, $5, $6)",
				id, owner, t.Name, t.Color, t.Notify, t.InstantMove,
			); err != nil {
				continue // name collision race or similar — skip rather than fail the whole import
			}
			tagIDByName[t.Name] = id
		}

		for _, mt := range b.MessageTags {
			tagID, ok := tagIDByName[mt.TagName]
			if !ok {
				continue
			}
			tx.Exec("INSERT INTO message_tags (message_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", mt.MessageID, tagID)
		}

		for _, ftr := range b.FolderTagRules {
			accountID, ok := accountIDByEmail[ftr.AccountEmail]
			tagID, tok := tagIDByName[ftr.TagName]
			if !ok || !tok {
				continue // account not connected here, or tag missing — nothing sane to attach this to
			}
			tx.Exec(`
				INSERT INTO folder_tag_rules (account_id, folder, tag_id) VALUES ($1, $2, $3)
				ON CONFLICT (account_id, folder) DO UPDATE SET tag_id = excluded.tag_id`,
				accountID, ftr.Folder, tagID)
		}

		for _, email := range b.TrustedSenders {
			tx.Exec("INSERT INTO trusted_senders (owner_subject, sender_email) VALUES ($1, $2) ON CONFLICT DO NOTHING", owner, email)
		}

		for _, tsb := range b.TagSenderBlocks {
			tagID, ok := tagIDByName[tsb.TagName]
			if !ok {
				continue
			}
			tx.Exec(
				"INSERT INTO tag_sender_blocks (owner_subject, sender_email, tag_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
				owner, tsb.SenderEmail, tagID)
		}
	}

	if include.history {
		// account_id is left NULL here — tag_history's own comment is explicit that it's
		// kept for reference/debugging only and never read by scoring, so there's nothing
		// worth resolving it for on restore. No de-dup key exists for this table (unlike
		// message_tags' real PK), so re-importing the same backup twice double-counts
		// these rows; acceptable for a restore-from-scratch flow, not for repeat imports.
		for _, h := range b.TagHistory {
			tagID, ok := tagIDByName[h.TagName]
			if !ok {
				continue
			}
			createdAt, err := time.Parse(time.RFC3339, h.CreatedAt)
			if err != nil {
				createdAt = time.Now()
			}
			subjectTokens := h.SubjectTokens
			if subjectTokens == nil {
				subjectTokens = []string{} // column is NOT NULL — a nil Go slice would otherwise bind as SQL NULL, not '{}'
			}
			tx.Exec(`
				INSERT INTO tag_history (id, owner_subject, message_id, tag_id, sender_email, sender_domain, subject_tokens, source, status, score, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				randomID(), owner, h.MessageID, tagID, h.SenderEmail, h.SenderDomain, subjectTokens, h.Source, h.Status, h.Score, createdAt)
		}

		for _, f := range b.SpamFlags {
			createdAt, err := time.Parse(time.RFC3339, f.CreatedAt)
			if err != nil {
				createdAt = time.Now()
			}
			tx.Exec(`
				INSERT INTO spam_flags (id, message_id, owner_subject, score, reasons, spf, dkim, dmarc, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				randomID(), f.MessageID, owner, f.Score, f.Reasons, f.SPF, f.DKIM, f.DMARC, createdAt)
		}
	}

	if include.settings && b.OwnerSettings.AutoTagMode != "" {
		tx.Exec(`
			INSERT INTO owner_settings (owner_subject, auto_tag_mode, spam_mode, auto_move_delay_days, image_cache_retention_days)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (owner_subject) DO UPDATE SET
				auto_tag_mode = excluded.auto_tag_mode, spam_mode = excluded.spam_mode,
				auto_move_delay_days = excluded.auto_move_delay_days,
				image_cache_retention_days = excluded.image_cache_retention_days`,
			owner, b.OwnerSettings.AutoTagMode, b.OwnerSettings.SpamMode, b.OwnerSettings.AutoMoveDelayDays, b.OwnerSettings.ImageCacheRetentionDays)
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The server has no notion of localStorage — handing these back lets the client
	// apply them itself instead of duplicating that list of keys server-side. Omitted
	// (not just empty) when settings weren't selected for import, so the client knows
	// not to touch localStorage at all rather than mistaking "nothing in the file" for
	// "intentionally excluded".
	resp := map[string]any{}
	if include.settings {
		resp["localPreferences"] = b.LocalPreferences
	}
	writeJSON(w, resp)
}
