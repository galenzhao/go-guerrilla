package backends

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const imapDialTimeout = 30 * time.Second

// imapDebugLogPath, when set via ALIAS_INDEX_IMAP_DEBUG, makes every IMAP
// connection dump raw wire traffic (including the plaintext LOGIN command,
// i.e. the account password) to that file for protocol-level debugging.
// Off by default; not something to leave on in normal operation.
var imapDebugLogPath = strings.TrimSpace(os.Getenv("ALIAS_INDEX_IMAP_DEBUG"))

// imapMailboxClient wraps go-imap for alias indexing.
type imapMailboxClient struct {
	client    *imapclient.Client
	mu        sync.Mutex
	debugFile *os.File
}

func dialIMAP(host string, port int, useTLS bool, handler *imapclient.UnilateralDataHandler) (*imapMailboxClient, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	opts := &imapclient.Options{
		Dialer: &net.Dialer{Timeout: imapDialTimeout},
	}
	if handler != nil {
		opts.UnilateralDataHandler = handler
	}
	var debugFile *os.File
	if imapDebugLogPath != "" {
		f, err := os.OpenFile(imapDebugLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			Log().WithError(err).WithField("path", imapDebugLogPath).Warn("alias-index imap debug log open failed")
		} else {
			debugFile = f
			opts.DebugWriter = f
			fmt.Fprintf(f, "\n===== dial %s (tls=%v) %s =====\n", addr, useTLS, time.Now().Format(time.RFC3339))
		}
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
		if debugFile != nil {
			_ = debugFile.Close()
		}
		return nil, err
	}
	return &imapMailboxClient{client: client, debugFile: debugFile}, nil
}

func (c *imapMailboxClient) Close() error {
	if c == nil {
		return nil
	}
	if c.debugFile != nil {
		_ = c.debugFile.Close()
	}
	if c.client == nil {
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
	UIDNext     uint32
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
		UIDNext:     uint32(data.UIDNext),
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

type imapFetchedMessage struct {
	UID     uint32
	Headers parsedMailHeaders
}

// FetchHeaderFields fetches header fields for an explicit list of UIDs.
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
	// Fetch the full header block (BODY[HEADER]) rather than a server-side
	// filtered subset (BODY[HEADER.FIELDS (...)]): the filtered form has been
	// observed returning an empty result against Tencent Exmail even for
	// mail that unambiguously has all of these headers, while a plain HEADER
	// fetch is one of the most universally-implemented IMAP operations.
	// parseMailHeaders already just looks up specific fields by name, so it
	// works the same either way — this only costs a few extra header bytes.
	bodySection := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierHeader,
		Peek:      true,
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
			Log().WithError(err).WithFields(map[string]interface{}{
				"uid":         uid,
				"header_size": len(headerBytes),
			}).Warn("alias-index imap header parse failed")
			continue
		}
		if headerBlockLooksEmpty(headers) {
			preview := string(headerBytes)
			if len(preview) > 200 {
				preview = preview[:200]
			}
			Log().WithFields(map[string]interface{}{
				"uid":         uid,
				"header_size": len(headerBytes),
				"preview":     preview,
			}).Debug("alias-index imap header fetch returned no indexable fields")
		}
		out = append(out, imapFetchedMessage{UID: uid, Headers: headers})
	}
	if err := fetchCmd.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *imapMailboxClient) Idle(ctx context.Context, wait func()) error {
	c.mu.Lock()
	idleCmd, err := c.client.Idle()
	c.mu.Unlock()
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- idleCmd.Wait()
	}()

	if wait != nil {
		wait()
	}

	_ = idleCmd.Close()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
