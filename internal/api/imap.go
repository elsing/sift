package api

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
)

const syncCount = 30 // most recent N messages to pull per account, per regular sync

// testIMAPLogin just confirms the credentials work, used when adding an account.
func testIMAPLogin(host string, port int, username, password string) error {
	c, err := imapclient.DialTLS(net.JoinHostPort(host, fmt.Sprint(port)), nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	if err := c.Login(username, password).Wait(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return nil
}

// syncIMAP fetches the most recent messages from INBOX and returns them as Mails,
// along with the lowest UID fetched (the boundary backfill continues below).
func syncIMAP(acct dbAccount, password string) ([]Mail, imap.UID, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return nil, 0, fmt.Errorf("login: %w", err)
	}

	mbox, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		return nil, 0, fmt.Errorf("select inbox: %w", err)
	}
	if mbox.NumMessages == 0 {
		return nil, 0, nil
	}

	start := uint32(1)
	if mbox.NumMessages > syncCount {
		start = mbox.NumMessages - syncCount + 1
	}
	seqSet := imap.SeqSetNum()
	seqSet.AddRange(start, mbox.NumMessages)

	fetchCmd := c.Fetch(seqSet, &imap.FetchOptions{
		Envelope: true,
		Flags:    true,
		UID:      true,
	})
	defer fetchCmd.Close()

	msgs, err := fetchCmd.Collect()
	if err != nil {
		return nil, 0, fmt.Errorf("fetch: %w", err)
	}

	mails, oldestUID := mailsFromFetch(acct.ID, "INBOX", msgs)
	return mails, oldestUID, nil
}

// backfillIMAP fetches up to batchSize messages older than beforeUID, for "load more"
// scrolling past what's already cached locally. Returns nil mails (not an error) once
// there's nothing older left.
func backfillIMAP(acct dbAccount, password string, beforeUID imap.UID, batchSize int) ([]Mail, imap.UID, error) {
	if beforeUID <= 1 {
		return nil, 0, nil // already at the oldest message
	}

	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return nil, 0, fmt.Errorf("login: %w", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return nil, 0, fmt.Errorf("select inbox: %w", err)
	}

	var olderThan imap.UIDSet
	olderThan.AddRange(1, beforeUID-1)
	searchData, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{olderThan}}, nil).Wait()
	if err != nil {
		return nil, 0, fmt.Errorf("search: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return nil, 0, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if len(uids) > batchSize {
		uids = uids[len(uids)-batchSize:]
	}

	fetchCmd := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		Envelope: true,
		Flags:    true,
		UID:      true,
	})
	defer fetchCmd.Close()

	msgs, err := fetchCmd.Collect()
	if err != nil {
		return nil, 0, fmt.Errorf("fetch: %w", err)
	}

	mails, oldestUID := mailsFromFetch(acct.ID, "INBOX", msgs)
	return mails, oldestUID, nil
}

// mailsFromFetch converts fetched IMAP messages to Mails. folder is baked into the ID
// (UIDs are only unique within a single mailbox, not across all of an account's folders).
func mailsFromFetch(accountID, folder string, msgs []*imapclient.FetchMessageBuffer) ([]Mail, imap.UID) {
	mails := make([]Mail, 0, len(msgs))
	oldestUID := imap.UID(0)
	for _, m := range msgs {
		if oldestUID == 0 || m.UID < oldestUID {
			oldestUID = m.UID
		}
		mails = append(mails, Mail{
			ID:          fmt.Sprintf("%s|%s|%d", accountID, folder, m.UID),
			Sender:      envelopeSender(m.Envelope),
			SenderEmail: envelopeSenderEmail(m.Envelope),
			Subject:     m.Envelope.Subject,
			Snippet:     "", // ponytail: body snippet needs a BODY[] fetch (extra round trip), add when needed
			Time:        formatMailTime(m.Envelope.Date),
			Date:        m.Envelope.Date.Format(time.RFC3339),
			Unread:      !slices.Contains(m.Flags, imap.FlagSeen),
			UID:         uint32(m.UID),
			Folder:      folder,
		})
	}
	return mails, oldestUID
}

// fetchFolderMail returns the most recent `limit` messages from an arbitrary mailbox
// (not just INBOX), for read-only folder browsing. Not cached/persisted locally.
func fetchFolderMail(acct dbAccount, password, folder string, limit int) ([]Mail, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}

	mbox, err := c.Select(folder, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", folder, err)
	}
	if mbox.NumMessages == 0 {
		return nil, nil
	}

	start := uint32(1)
	if mbox.NumMessages > uint32(limit) {
		start = mbox.NumMessages - uint32(limit) + 1
	}
	seqSet := imap.SeqSetNum()
	seqSet.AddRange(start, mbox.NumMessages)

	fetchCmd := c.Fetch(seqSet, &imap.FetchOptions{Envelope: true, Flags: true, UID: true})
	defer fetchCmd.Close()
	msgs, err := fetchCmd.Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	mails, _ := mailsFromFetch(acct.ID, folder, msgs)
	return mails, nil
}

// formatMailTime shows a bare time for today's mail, and a date for anything older.
func formatMailTime(t time.Time) string {
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("3:04 PM")
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// listFolders returns every mailbox name on the account and the server's hierarchy delimiter.
func listFolders(host string, port int, username, password string) ([]string, rune, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(host, fmt.Sprint(port)), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	if err := c.Login(username, password).Wait(); err != nil {
		return nil, 0, fmt.Errorf("login: %w", err)
	}

	data, err := c.List("", "*", nil).Collect()
	if err != nil {
		return nil, 0, fmt.Errorf("list: %w", err)
	}

	var names []string
	var delim rune
	for _, d := range data {
		names = append(names, d.Mailbox)
		delim = d.Delim
	}
	return names, delim, nil
}

type folderNode struct {
	Name     string        `json:"name"`
	Path     string        `json:"path"`
	Children []*folderNode `json:"children,omitempty"`
}

// buildFolderTree nests flat IMAP mailbox names (e.g. "Work/Invoices") under their
// hierarchy delimiter, preserving the server's reported order.
func buildFolderTree(names []string, delim rune) []*folderNode {
	sep := string(delim)
	if sep == "" {
		sep = "/" // ponytail: fallback if the server reports no delimiter; flat namespace either way
	}

	type builder struct {
		node     *folderNode
		children map[string]*builder
		order    []string
	}
	root := &builder{children: map[string]*builder{}}

	for _, full := range names {
		parts := strings.Split(full, sep)
		cur := root
		var pathSoFar []string
		for _, part := range parts {
			pathSoFar = append(pathSoFar, part)
			b, ok := cur.children[part]
			if !ok {
				b = &builder{
					node:     &folderNode{Name: part, Path: strings.Join(pathSoFar, sep)},
					children: map[string]*builder{},
				}
				cur.children[part] = b
				cur.order = append(cur.order, part)
			}
			cur = b
		}
	}

	var toNodes func(*builder) []*folderNode
	toNodes = func(b *builder) []*folderNode {
		out := make([]*folderNode, 0, len(b.order))
		for _, name := range b.order {
			child := b.children[name]
			child.node.Children = toNodes(child)
			out = append(out, child.node)
		}
		return out
	}
	return toNodes(root)
}

func envelopeSender(env *imap.Envelope) string {
	if env == nil || len(env.From) == 0 {
		return "(unknown)"
	}
	from := env.From[0]
	if from.Name != "" {
		return from.Name
	}
	return from.Mailbox + "@" + from.Host
}

// envelopeSenderEmail returns the actual address, even when envelopeSender shows a
// display name instead — e.g. for the reveal-and-copy affordance on a mail row.
func envelopeSenderEmail(env *imap.Envelope) string {
	if env == nil || len(env.From) == 0 {
		return ""
	}
	from := env.From[0]
	return from.Mailbox + "@" + from.Host
}

// detectSpecialUseFolders finds the account's Archive and Trash mailboxes. It prefers
// the SPECIAL-USE extension (RFC 6154); servers that don't support it (common on
// self-hosted Dovecot) fall back to matching common folder names. Either return value
// can come back empty if nothing matched — that's not an error, just "not found".
func detectSpecialUseFolders(host string, port int, username, password string) (archive, trash string, err error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(host, fmt.Sprint(port)), nil)
	if err != nil {
		return "", "", fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	if err := c.Login(username, password).Wait(); err != nil {
		return "", "", fmt.Errorf("login: %w", err)
	}

	data, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return "", "", fmt.Errorf("list: %w", err)
	}

	for _, d := range data {
		if slices.Contains(d.Attrs, imap.MailboxAttrArchive) {
			archive = d.Mailbox
		}
		if slices.Contains(d.Attrs, imap.MailboxAttrTrash) {
			trash = d.Mailbox
		}
	}
	if archive == "" {
		archive = matchFolderName(data, "archive")
	}
	if trash == "" {
		trash = matchFolderName(data, "trash", "deleted")
	}
	return archive, trash, nil
}

func matchFolderName(data []*imap.ListData, needles ...string) string {
	for _, d := range data {
		lower := strings.ToLower(d.Mailbox)
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				return d.Mailbox
			}
		}
	}
	return ""
}

// moveMail moves a single message (by UID, within sourceFolder) to destFolder.
func moveMail(host string, port int, username, password string, uid uint32, sourceFolder, destFolder string) error {
	c, err := imapclient.DialTLS(net.JoinHostPort(host, fmt.Sprint(port)), nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	if err := c.Login(username, password).Wait(); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if _, err := c.Select(sourceFolder, nil).Wait(); err != nil {
		return fmt.Errorf("select %s: %w", sourceFolder, err)
	}
	if _, err := c.Move(imap.UIDSetNum(imap.UID(uid)), destFolder).Wait(); err != nil {
		return fmt.Errorf("move: %w", err)
	}
	return nil
}

// MailBody holds whichever parts of a message we could extract. The client renders
// HTML (sandboxed, scripts disabled) when present, falling back to plain text.
type MailBody struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`
}

// fetchMailBody fetches the raw message and extracts its text/plain and text/html
// parts (whichever exist — a message may have one, both, or neither).
// ponytail: no inline images/attachments resolved into the HTML yet — cid: references
// will show as broken images. Add a part-fetching pass if that's needed.
func fetchMailBody(acct dbAccount, password, folder string, uid uint32) (MailBody, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return MailBody{}, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()
	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return MailBody{}, fmt.Errorf("login: %w", err)
	}
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		return MailBody{}, fmt.Errorf("select %s: %w", folder, err)
	}

	fetchCmd := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	})
	defer fetchCmd.Close()
	msgs, err := fetchCmd.Collect()
	if err != nil {
		return MailBody{}, fmt.Errorf("fetch: %w", err)
	}
	if len(msgs) == 0 || len(msgs[0].BodySection) == 0 {
		return MailBody{}, fmt.Errorf("message not found")
	}

	mr, err := mail.CreateReader(bytes.NewReader(msgs[0].BodySection[0].Bytes))
	if mr == nil {
		return MailBody{}, fmt.Errorf("parse message: %w", err)
	}

	var body MailBody
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // best-effort: stop at the first unparseable part rather than failing the whole body
		}
		if _, ok := part.Header.(*mail.InlineHeader); !ok {
			continue // skip attachments
		}
		contentType, _, _ := part.Header.(*mail.InlineHeader).ContentType()
		raw, _ := io.ReadAll(part.Body)
		switch {
		case contentType == "text/plain" && body.Text == "":
			body.Text = string(raw)
		case contentType == "text/html" && body.HTML == "":
			body.HTML = string(raw)
		}
	}
	if body.Text == "" && body.HTML == "" {
		body.Text = "(no readable body)"
	}
	return body, nil
}
