package backends

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	defaultIMAPIdleRefresh    = 28 * time.Minute
	defaultIMAPResyncInterval = 5 * time.Minute
	defaultIMAPSearchBatch    = 500
)

// AliasIMAPAccount is one IMAP mailbox to watch for alias indexing.
type AliasIMAPAccount struct {
	Host     string
	Port     int
	TLS      bool
	User     string
	Password string
	TenantID string
}

// MailboxKey returns the stable key for IMAP cursor and seen-message storage.
func (a AliasIMAPAccount) MailboxKey() string {
	return fmt.Sprintf("imap:%s@%s", strings.TrimSpace(a.User), strings.TrimSpace(a.Host))
}

func (a AliasIMAPAccount) validate() error {
	if strings.TrimSpace(a.Host) == "" {
		return fmt.Errorf("alias_index imap host is required")
	}
	if strings.TrimSpace(a.User) == "" {
		return fmt.Errorf("alias_index imap user is required for host %s", a.Host)
	}
	return nil
}

// IMAPWatcherConfig configures IMAP IDLE watching.
type IMAPWatcherConfig struct {
	IdleRefresh    time.Duration
	ResyncInterval time.Duration
	SearchBatch    int
	ExcludeFolders map[string]struct{}
}

func (c *IMAPWatcherConfig) normalize() {
	if c.IdleRefresh <= 0 {
		c.IdleRefresh = defaultIMAPIdleRefresh
	}
	if c.ResyncInterval <= 0 {
		c.ResyncInterval = defaultIMAPResyncInterval
	}
	if c.SearchBatch <= 0 {
		c.SearchBatch = defaultIMAPSearchBatch
	}
}

// IMAPAccountWatcher watches all folders on one IMAP account.
type IMAPAccountWatcher struct {
	account AliasIMAPAccount
	cfg     IMAPWatcherConfig
	indexer *AliasIndexer
}

func newIMAPAccountWatcher(account AliasIMAPAccount, cfg IMAPWatcherConfig, indexer *AliasIndexer) *IMAPAccountWatcher {
	cfg.normalize()
	return &IMAPAccountWatcher{account: account, cfg: cfg, indexer: indexer}
}

func (w *IMAPAccountWatcher) Run(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()
}

func (w *IMAPAccountWatcher) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		client, err := dialIMAP(w.account.Host, w.account.Port, w.account.TLS, nil)
		if err != nil {
			Log().WithError(err).WithField("mailbox", w.account.MailboxKey()).Error("alias-index imap dial failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if err := client.Login(w.account.User, w.account.Password); err != nil {
			_ = client.Close()
			Log().WithError(err).WithField("mailbox", w.account.MailboxKey()).Error("alias-index imap login failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		folders, err := client.ListFolders()
		_ = client.Close()
		if err != nil {
			Log().WithError(err).WithField("mailbox", w.account.MailboxKey()).Error("alias-index imap list folders failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		folders = w.filterFolders(folders)
		if len(folders) == 0 {
			Log().WithField("mailbox", w.account.MailboxKey()).Warn("alias-index imap found no watchable folders")
			if !sleepContext(ctx, time.Minute) {
				return
			}
			continue
		}
		Log().WithFields(map[string]interface{}{
			"mailbox": w.account.MailboxKey(),
			"folders": len(folders),
		}).Info("alias-index imap watching folders")

		var folderWG sync.WaitGroup
		for _, folder := range folders {
			folder := folder
			folderWG.Add(1)
			go func() {
				defer folderWG.Done()
				w.watchFolder(ctx, folder)
			}()
		}
		folderWG.Wait()
		if ctx.Err() != nil {
			return
		}
		if !sleepContext(ctx, 30*time.Second) {
			return
		}
	}
}

func (w *IMAPAccountWatcher) filterFolders(folders []string) []string {
	out := make([]string, 0, len(folders))
	for _, folder := range folders {
		if _, excluded := w.cfg.ExcludeFolders[folder]; excluded {
			continue
		}
		out = append(out, folder)
	}
	return out
}

func (w *IMAPAccountWatcher) watchFolder(ctx context.Context, folder string) {
	mailboxKey := w.account.MailboxKey()
	for {
		if ctx.Err() != nil {
			return
		}
		client, err := dialIMAP(w.account.Host, w.account.Port, w.account.TLS, nil)
		if err != nil {
			Log().WithError(err).WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
			}).Error("alias-index imap folder dial failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if err := client.Login(w.account.User, w.account.Password); err != nil {
			_ = client.Close()
			Log().WithError(err).WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
			}).Error("alias-index imap folder login failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		selected, err := client.SelectFolder(folder)
		if err != nil {
			_ = client.Close()
			Log().WithError(err).WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
			}).Error("alias-index imap select folder failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}

		cursor, err := w.indexer.store.GetIMAPCursor(mailboxKey, folder)
		if err != nil {
			_ = client.Close()
			Log().WithError(err).WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
			}).Error("alias-index imap cursor read failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if cursor != nil && cursor.UIDValidity != 0 && cursor.UIDValidity != selected.UIDValidity {
			_ = w.indexer.store.DeleteIMAPCursor(mailboxKey, folder)
			cursor = nil
			Log().WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
			}).Info("alias-index imap uidvalidity changed; resetting folder cursor")
		}

		needsBaseline := w.indexer.cfg.SkipExistingOnStart && (cursor == nil || !cursor.BaselineDone)
		if needsBaseline {
			if err := w.baselineFolder(client, mailboxKey, folder, selected.UIDValidity); err != nil {
				_ = client.Close()
				Log().WithError(err).WithFields(map[string]interface{}{
					"mailbox": mailboxKey,
					"folder":  folder,
				}).Error("alias-index imap baseline failed")
				if !sleepContext(ctx, 30*time.Second) {
					return
				}
				continue
			}
			cursor, err = w.indexer.store.GetIMAPCursor(mailboxKey, folder)
			if err != nil || cursor == nil {
				_ = client.Close()
				if !sleepContext(ctx, 30*time.Second) {
					return
				}
				continue
			}
		}

		var lastUID uint32
		if cursor != nil {
			lastUID = cursor.LastUID
		}
		if indexed, err := w.processUIDs(client, mailboxKey, folder, selected.UIDValidity, lastUID+1); err != nil {
			_ = client.Close()
			Log().WithError(err).WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
			}).Error("alias-index imap sync failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		} else if indexed > 0 {
			Log().WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  folder,
				"indexed": indexed,
			}).Info("alias-index imap indexed new messages")
			if cur, err := w.indexer.store.GetIMAPCursor(mailboxKey, folder); err == nil && cur != nil {
				lastUID = cur.LastUID
			}
		}

		for ctx.Err() == nil {
			wakeup := make(chan struct{}, 1)
			_ = client.Close()
			client, err = dialIMAP(w.account.Host, w.account.Port, w.account.TLS, &imapclient.UnilateralDataHandler{
				Mailbox: func(data *imapclient.UnilateralDataMailbox) {
					if data.NumMessages != nil {
						select {
						case wakeup <- struct{}{}:
						default:
						}
					}
				},
			})
			if err != nil {
				break
			}
			if err := client.Login(w.account.User, w.account.Password); err != nil {
				_ = client.Close()
				break
			}
			if _, err := client.SelectFolder(folder); err != nil {
				_ = client.Close()
				break
			}

			idleDone := make(chan error, 1)
			go func() {
				idleDone <- client.Idle(func() {
					idleTimer := time.NewTimer(w.cfg.IdleRefresh)
					defer idleTimer.Stop()
					select {
					case <-ctx.Done():
					case <-idleTimer.C:
					case <-wakeup:
					}
				})
			}()

			select {
			case <-ctx.Done():
				_ = client.Close()
				return
			case <-wakeup:
			case <-time.After(w.cfg.ResyncInterval):
			case err := <-idleDone:
				if err != nil && ctx.Err() == nil {
					Log().WithError(err).WithFields(map[string]interface{}{
						"mailbox": mailboxKey,
						"folder":  folder,
					}).Debug("alias-index imap idle ended")
				}
			}
			_ = client.Close()

			client, err = dialIMAP(w.account.Host, w.account.Port, w.account.TLS, nil)
			if err != nil {
				break
			}
			if err := client.Login(w.account.User, w.account.Password); err != nil {
				_ = client.Close()
				break
			}
			if _, err := client.SelectFolder(folder); err != nil {
				_ = client.Close()
				break
			}
			if indexed, err := w.processUIDs(client, mailboxKey, folder, selected.UIDValidity, lastUID+1); err != nil {
				Log().WithError(err).WithFields(map[string]interface{}{
					"mailbox": mailboxKey,
					"folder":  folder,
				}).Warn("alias-index imap post-idle sync failed")
				break
			} else if indexed > 0 {
				Log().WithFields(map[string]interface{}{
					"mailbox": mailboxKey,
					"folder":  folder,
					"indexed": indexed,
				}).Info("alias-index imap indexed new messages")
			}
			if cur, err := w.indexer.store.GetIMAPCursor(mailboxKey, folder); err == nil && cur != nil {
				lastUID = cur.LastUID
			}
		}
		_ = client.Close()
	}
}

func (w *IMAPAccountWatcher) baselineFolder(client *imapMailboxClient, mailboxKey, folder string, uidValidity uint32) error {
	uids, err := client.UIDSearchAll()
	if err != nil {
		return err
	}
	messageIDs := make([]string, 0, len(uids))
	var maxUID uint32
	for start := 0; start < len(uids); start += w.cfg.SearchBatch {
		end := start + w.cfg.SearchBatch
		if end > len(uids) {
			end = len(uids)
		}
		batch := uids[start:end]
		messages, err := client.FetchHeaderFields(batch)
		if err != nil {
			return err
		}
		for _, msg := range messages {
			if msg.UID > maxUID {
				maxUID = msg.UID
			}
			if id := normalizeMessageID(msg.Headers.MessageID); id != "" {
				messageIDs = append(messageIDs, id)
			}
		}
	}
	if err := w.indexer.store.MarkSeenMessages(mailboxKey, messageIDs); err != nil {
		return err
	}
	if err := w.indexer.store.SetIMAPCursor(IMAPCursor{
		Mailbox:      mailboxKey,
		Folder:       folder,
		UIDValidity:  uidValidity,
		LastUID:      maxUID,
		BaselineDone: true,
	}); err != nil {
		return err
	}
	Log().WithFields(map[string]interface{}{
		"mailbox":    mailboxKey,
		"folder":     folder,
		"uidl_count": len(uids),
	}).Info("alias-index imap recorded folder baseline; existing mail skipped")
	return nil
}

func (w *IMAPAccountWatcher) processUIDs(client *imapMailboxClient, mailboxKey, folder string, uidValidity, fromUID uint32) (int, error) {
	uids, err := client.UIDSearchFrom(fromUID)
	if err != nil {
		return 0, err
	}
	if len(uids) == 0 {
		return 0, nil
	}
	indexed := 0
	var maxUID uint32
	for start := 0; start < len(uids); start += w.cfg.SearchBatch {
		end := start + w.cfg.SearchBatch
		if end > len(uids) {
			end = len(uids)
		}
		batch := uids[start:end]
		messages, err := client.FetchHeaderFields(batch)
		if err != nil {
			return indexed, err
		}
		for _, msg := range messages {
			if msg.UID > maxUID {
				maxUID = msg.UID
			}
			outcome, err := indexMessageFromHeaders(w.indexer.store, msg.Headers, indexMessageOpts{
				mailboxKey:  mailboxKey,
				mailboxUser: w.account.User,
				tenantID:    w.account.TenantID,
				since:       w.indexer.cfg.Since,
				folder:      folder,
			})
			if err != nil {
				return indexed, err
			}
			if outcome == outcomeIndexed {
				indexed++
			}
		}
	}
	cursor, err := w.indexer.store.GetIMAPCursor(mailboxKey, folder)
	if err != nil {
		return indexed, err
	}
	lastUID := maxUID
	baselineDone := true
	if cursor != nil {
		if cursor.LastUID > lastUID {
			lastUID = cursor.LastUID
		}
		baselineDone = cursor.BaselineDone
	}
	if maxUID > 0 {
		if err := w.indexer.store.SetIMAPCursor(IMAPCursor{
			Mailbox:      mailboxKey,
			Folder:       folder,
			UIDValidity:  uidValidity,
			LastUID:      lastUID,
			BaselineDone: baselineDone,
		}); err != nil {
			return indexed, err
		}
	}
	return indexed, nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
