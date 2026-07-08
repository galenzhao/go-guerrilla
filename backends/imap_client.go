package backends

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

var imapHeaderFields = []string{
	"Message-ID", "From", "To", "Cc", "Delivered-To", "X-Original-To", "Date",
}

// imapMailboxClient wraps go-imap for alias indexing.
type imapMailboxClient struct {
	client *imapclient.Client
	mu     sync.Mutex
}

func dialIMAP(host string, port int, useTLS bool, handler *imapclient.UnilateralDataHandler) (*imapMailboxClient, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	opts := &imapclient.Options{}
	if handler != nil {
		opts.UnilateralDataHandler = handler
	}
	var (
		client *imapclient.Client
		err    error
	)
	if useTLS {
		client, err = imapclient.DialTLS(addr, opts)
	} else {
		client, err = imapclient.DialStartTLS(addr, opts)
	}
	if err != nil {
		return nil, err
	}
	return &imapMailboxClient{client: client}, nil
}

func (c *imapMailboxClient) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

func (c *imapMailboxClient) Login(user, password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Login(user, password).Wait()
}

func (c *imapMailboxClient) ListFolders() ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mailboxes, err := c.client.List("", "*", nil).Collect()
	if err != nil {
		return nil, err
	}
	folders := make([]string, 0, len(mailboxes))
	for _, mbox := range mailboxes {
		if hasIMAPAttr(mbox.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		name := strings.TrimSpace(mbox.Mailbox)
		if name == "" {
			continue
		}
		folders = append(folders, name)
	}
	return folders, nil
}

func hasIMAPAttr(attrs []imap.MailboxAttr, target imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if attr == target {
			return true
		}
	}
	return false
}

type imapSelectResult struct {
	UIDValidity uint32
	NumMessages uint32
}

func (c *imapMailboxClient) SelectFolder(folder string) (*imapSelectResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := c.client.Select(folder, nil).Wait()
	if err != nil {
		return nil, err
	}
	return &imapSelectResult{
		UIDValidity: uint32(data.UIDValidity),
		NumMessages: data.NumMessages,
	}, nil
}

func (c *imapMailboxClient) UIDSearchAll() ([]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := c.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := data.AllUIDs()
	out := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		out = append(out, uint32(uid))
	}
	return out, nil
}

func (c *imapMailboxClient) UIDSearchFrom(fromUID uint32) ([]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var uidSet imap.UIDSet
	uidSet.AddRange(imap.UID(fromUID), 0)
	criteria := &imap.SearchCriteria{
		UID: []imap.UIDSet{uidSet},
	}
	data, err := c.client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := data.AllUIDs()
	out := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if uint32(uid) >= fromUID {
			out = append(out, uint32(uid))
		}
	}
	return out, nil
}

type imapFetchedMessage struct {
	UID     uint32
	Headers parsedMailHeaders
}

func (c *imapMailboxClient) FetchHeaderFields(uids []uint32) ([]imapFetchedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	uidSet := imap.UIDSetNum()
	for _, uid := range uids {
		uidSet.AddNum(imap.UID(uid))
	}
	bodySection := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: imapHeaderFields,
		Peek:         true,
	}
	fetchCmd := c.client.Fetch(uidSet, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	})
	defer fetchCmd.Close()

	var out []imapFetchedMessage
	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}
		var uid uint32
		var headerBytes []byte
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch data := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = uint32(data.UID)
			case imapclient.FetchItemDataBodySection:
				b, err := io.ReadAll(data.Literal)
				if err != nil {
					return nil, err
				}
				headerBytes = b
			}
		}
		if uid == 0 {
			continue
		}
		headers, err := parseMailHeaders(string(headerBytes))
		if err != nil {
			Log().WithError(err).WithField("uid", uid).Warn("alias-index imap header parse failed")
			continue
		}
		out = append(out, imapFetchedMessage{UID: uid, Headers: headers})
	}
	if err := fetchCmd.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *imapMailboxClient) Idle(wait func()) error {
	c.mu.Lock()
	idleCmd, err := c.client.Idle()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	defer idleCmd.Close()

	done := make(chan error, 1)
	go func() {
		done <- idleCmd.Wait()
	}()

	if wait != nil {
		wait()
	}

	if err := idleCmd.Close(); err != nil {
		return err
	}
	return <-done
}
