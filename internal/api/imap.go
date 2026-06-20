package api

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const syncCount = 30 // most recent N messages to pull per account, per sync

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

// syncIMAP fetches the most recent messages from INBOX and returns them as Mails.
func syncIMAP(acct dbAccount, password string) ([]Mail, error) {
	c, err := imapclient.DialTLS(net.JoinHostPort(acct.Host, fmt.Sprint(acct.Port)), nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	if err := c.Login(acct.Username, password).Wait(); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}

	mbox, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select inbox: %w", err)
	}
	if mbox.NumMessages == 0 {
		return nil, nil
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
		return nil, fmt.Errorf("fetch: %w", err)
	}

	mails := make([]Mail, 0, len(msgs))
	for _, m := range msgs {
		mails = append(mails, Mail{
			ID:      fmt.Sprintf("%s-%d", acct.ID, m.UID),
			Sender:  envelopeSender(m.Envelope),
			Subject: m.Envelope.Subject,
			Snippet: "", // ponytail: body snippet needs a BODY[] fetch (extra round trip), add when needed
			Time:    formatMailTime(m.Envelope.Date),
			Unread:  !slices.Contains(m.Flags, imap.FlagSeen),
		})
	}
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
