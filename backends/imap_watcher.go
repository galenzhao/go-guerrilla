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
	defaultIMAPIdleRefresh     = 28 * time.Minute
	defaultIMAPResyncInterval  = 5 * time.Minute
	defaultIMAPSearchBatch     = 500
	defaultBaselineHeaderLimit = 200
	imapFolderOpTimeout        = 45 * time.Second
	imapSessionTimeoutCap      = 3 * time.Minute
	imapEmptyHeaderMaxRetries  = 3
	imapEmptyHeaderRetryDelay  = 4 * time.Second
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
	IdleRefresh         time.Duration
	ResyncInterval      time.Duration
	SearchBatch         int
	BaselineHeaderLimit int
	ExcludeFolders      map[string]struct{}
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
	if c.BaselineHeaderLimit <= 0 {
		c.BaselineHeaderLimit = defaultBaselineHeaderLimit
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

func (w *IMAPAccountWatcher) logIMAPError(phase, folder string, err error) {
	if err == nil {
		return
	}
	fields := map[string]interface{}{
		"mailbox": w.account.MailboxKey(),
		"phase":   phase,
	}
	if folder != "" {
		fields["folder"] = folder
	}
	Log().WithError(err).WithFields(fields).Error("alias-index imap error")
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

		// Login and ListFolders read from the connection with no deadline of
		// their own (only the initial TCP dial above is bounded), so a server
		// that accepts the connection but stalls answering LOGIN would hang
		// this goroutine forever with no error and no retry. Bound the whole
		// handshake and force the connection closed if it runs long.
		var folders []string
		opErr := runWithIMAPTimeout(ctx, client, imapFolderOpTimeout, func() error {
			if err := client.Login(w.account.User, w.account.Password); err != nil {
				return fmt.Errorf("login: %w", err)
			}
			listed, err := client.ListFolders()
			if err != nil {
				return fmt.Errorf("list folders: %w", err)
			}
			folders = listed
			return nil
		})
		_ = client.Close()
		if opErr != nil {
			Log().WithError(opErr).WithField("mailbox", w.account.MailboxKey()).Error("alias-index imap connect failed")
			if !sleepContext(ctx, 30*time.Second) {
				return
			}
			continue
		}
		folders = prioritizeIMAPFolders(w.filterFolders(folders))
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
			"names":   folders,
		}).Info("alias-index imap watching folders")

		w.watchAccount(ctx, folders)
		if ctx.Err() != nil {
			return
		}
		if !sleepContext(ctx, 30*time.Second) {
			return
		}
	}
}

func (w *IMAPAccountWatcher) watchAccount(ctx context.Context, folders []string) {
	mailboxKey := w.account.MailboxKey()

	resyncTicker := time.NewTicker(w.cfg.ResyncInterval)
	defer resyncTicker.Stop()

	// IDLE only reports changes on the currently selected folder, so we hold
	// it open on the highest-priority folder (INBOX when present) and rely on
	// resyncTicker as a fallback to catch changes in the other folders.
	idleFolder := folders[0]

	for {
		if ctx.Err() != nil {
			return
		}
		w.syncAllFolders(ctx, folders)
		if ctx.Err() != nil {
			return
		}

		idleCtx, cancelIdle := context.WithCancel(ctx)
		notify := make(chan struct{}, 1)
		idleDone := make(chan error, 1)
		go func() {
			idleDone <- w.runIdleSession(idleCtx, idleFolder, notify)
		}()

		select {
		case <-ctx.Done():
			cancelIdle()
			w.waitIdleDone(idleDone, idleFolder)
			return
		case <-notify:
			Log().WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folder":  idleFolder,
			}).Info("alias-index imap idle detected new activity")
			cancelIdle()
			w.waitIdleDone(idleDone, idleFolder)
		case <-resyncTicker.C:
			Log().WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"folders": len(folders),
			}).Info("alias-index imap resync")
			cancelIdle()
			w.waitIdleDone(idleDone, idleFolder)
		case err := <-idleDone:
			cancelIdle()
			if err != nil && ctx.Err() == nil {
				w.logIMAPError("idle", idleFolder, err)
				sleepContext(ctx, 5*time.Second)
			}
		}
	}
}

// waitIdleDone waits for a cancelled idle session to close its connection and
// return. Closing an IDLE command over a network that has gone dark (no
// TCP-level error, just a stalled peer) can block indefinitely with no
// deadline set on the socket, so this bounds the wait: past imapFolderOpTimeout
// we stop waiting and let the account watcher move on to the next cycle. The
// abandoned goroutine will exit on its own once the OS eventually reports the
// dead connection.
func (w *IMAPAccountWatcher) waitIdleDone(idleDone <-chan error, folder string) {
	select {
	case <-idleDone:
	case <-time.After(imapFolderOpTimeout):
		Log().WithFields(map[string]interface{}{
			"mailbox": w.account.MailboxKey(),
			"folder":  folder,
		}).Warn("alias-index imap idle session did not close in time; abandoning it")
	}
}

// runIdleSession opens a dedicated connection and holds an IMAP IDLE command
// on folder until ctx is cancelled, the idle refresh timer fires, or the
// server reports a mailbox change (new/expunged message), in which case it
// signals notify and returns.
func (w *IMAPAccountWatcher) runIdleSession(ctx context.Context, folder string, notify chan<- struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	signal := func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	handler := &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				signal()
			}
		},
		Expunge: func(seqNum uint32) {
			signal()
		},
	}

	client, err := dialIMAP(w.account.Host, w.account.Port, w.account.TLS, handler)
	if err != nil {
		return err
	}
	defer client.Close()

	// Same reasoning as run(): Login/SelectFolder have no read deadline of
	// their own, so bound them explicitly rather than risk leaking this
	// goroutine and its connection forever on a server that stalls here.
	if err := runWithIMAPTimeout(ctx, client, imapFolderOpTimeout, func() error {
		if err := client.Login(w.account.User, w.account.Password); err != nil {
			return err
		}
		_, err := client.SelectFolder(folder)
		return err
	}); err != nil {
		return err
	}

	refresh := time.NewTimer(w.cfg.IdleRefresh)
	defer refresh.Stop()

	return client.Idle(ctx, func() {
		select {
		case <-ctx.Done():
		case <-refresh.C:
		}
	})
}

func (w *IMAPAccountWatcher) syncAllFolders(ctx context.Context, folders []string) {
	client, err := dialIMAP(w.account.Host, w.account.Port, w.account.TLS, nil)
	if err != nil {
		w.logIMAPError("dial", "", err)
		sleepContext(ctx, 30*time.Second)
		return
	}

	sessionTimeout := imapFolderOpTimeout * time.Duration(len(folders))
	if sessionTimeout > imapSessionTimeoutCap {
		sessionTimeout = imapSessionTimeoutCap
	}

	var failed int
	opErr := runWithIMAPTimeout(ctx, client, sessionTimeout, func() error {
		if err := client.Login(w.account.User, w.account.Password); err != nil {
			return fmt.Errorf("login: %w", err)
		}
		for _, folder := range folders {
			if err := w.syncFolderOnClient(ctx, client, folder); err != nil {
				w.logIMAPError("folder", folder, err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d folder(s) failed", failed, len(folders))
		}
		return nil
	})
	_ = client.Close()

	if opErr != nil && ctx.Err() == nil {
		w.logIMAPError("session", "", opErr)
		sleepContext(ctx, 30*time.Second)
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

func prioritizeIMAPFolders(folders []string) []string {
	if len(folders) <= 1 {
		return folders
	}
	out := make([]string, 0, len(folders))
	rest := make([]string, 0, len(folders))
	for _, folder := range folders {
		if strings.EqualFold(strings.TrimSpace(folder), "INBOX") {
			out = append(out, folder)
		} else {
			rest = append(rest, folder)
		}
	}
	return append(out, rest...)
}

func (w *IMAPAccountWatcher) syncFolderOnClient(ctx context.Context, client *imapMailboxClient, folder string) error {
	mailboxKey := w.account.MailboxKey()

	selected, err := client.SelectFolder(folder)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}

	cursor, err := w.indexer.store.GetIMAPCursor(mailboxKey, folder)
	if err != nil {
		return fmt.Errorf("cursor: %w", err)
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
		if err := w.baselineFolder(ctx, client, mailboxKey, folder, selected.UIDValidity); err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
		selected, err = client.SelectFolder(folder)
		if err != nil {
			return fmt.Errorf("re-select after baseline: %w", err)
		}
		cursor, err = w.indexer.store.GetIMAPCursor(mailboxKey, folder)
		if err != nil || cursor == nil {
			return fmt.Errorf("cursor after baseline: %w", err)
		}
	}

	indexed, _, err := w.syncFolderUIDs(ctx, client, mailboxKey, folder, selected, cursor)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if indexed > 0 {
		Log().WithFields(map[string]interface{}{
			"mailbox": mailboxKey,
			"folder":  folder,
			"indexed": indexed,
		}).Info("alias-index imap indexed new messages")
	}
	return nil
}

func runWithIMAPTimeout(ctx context.Context, client *imapMailboxClient, d time.Duration, fn func() error) error {
	if d <= 0 {
		return fn()
	}
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = client.Close()
		return ctx.Err()
	case <-time.After(d):
		_ = client.Close()
		return fmt.Errorf("imap operation timed out after %s", d)
	}
}

func (w *IMAPAccountWatcher) baselineFolder(ctx context.Context, client *imapMailboxClient, mailboxKey, folder string, uidValidity uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uids, err := client.UIDSearchAll()
	if err != nil {
		return err
	}
	maxUID := maxIMAPUID(uids)
	if len(uids) > w.cfg.BaselineHeaderLimit {
		return w.baselineFolderCursorOnly(mailboxKey, folder, uidValidity, maxUID, len(uids))
	}
	preMaxUID := maxUID
	messageIDs := make([]string, 0, len(uids))
	var fetchedMaxUID uint32
	for start := 0; start < len(uids); start += w.cfg.SearchBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			if msg.UID > fetchedMaxUID {
				fetchedMaxUID = msg.UID
			}
			// Only mark mail present at baseline start as seen. Messages arriving
			// during baseline keep UIDs above preMaxUID and should be indexed next.
			if msg.UID > preMaxUID {
				continue
			}
			if id := normalizeMessageID(msg.Headers.MessageID); id != "" {
				messageIDs = append(messageIDs, id)
			}
		}
	}
	if fetchedMaxUID > maxUID {
		maxUID = fetchedMaxUID
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

func (w *IMAPAccountWatcher) baselineFolderCursorOnly(mailboxKey, folder string, uidValidity, maxUID uint32, uidCount int) error {
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
		"mailbox":     mailboxKey,
		"folder":      folder,
		"uidl_count":  uidCount,
		"cursor_only": true,
	}).Info("alias-index imap recorded folder baseline; existing mail skipped")
	return nil
}

func maxIMAPUID(uids []uint32) uint32 {
	var max uint32
	for _, uid := range uids {
		if uid > max {
			max = uid
		}
	}
	return max
}

// newMailUIDRange returns the inclusive UID range of mail not yet indexed,
// derived from the last UID we recorded and the folder's current UIDNEXT.
// IMAP assigns UIDs within a folder in strictly ascending order (RFC 3501
// §2.3.1.1), and UIDNEXT is guaranteed to exceed every UID currently in the
// folder, so this range always covers every message added since lastUID —
// whether delivered directly or moved in by a server-side rule — without
// needing a SEARCH to discover it.
func newMailUIDRange(lastUID, uidNext uint32) (fromUID, toUID uint32, hasNew bool) {
	fromUID = lastUID + 1
	if uidNext == 0 || uidNext-1 < fromUID {
		return fromUID, 0, false
	}
	return fromUID, uidNext - 1, true
}

func (w *IMAPAccountWatcher) syncFolderUIDs(ctx context.Context, client *imapMailboxClient, mailboxKey, folder string, selected *imapSelectResult, cursor *IMAPCursor) (int, uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	lastUID := uint32(0)
	if cursor != nil {
		lastUID = cursor.LastUID
	}

	fromUID, toUID, hasNew := newMailUIDRange(lastUID, selected.UIDNext)
	if !hasNew {
		Log().WithFields(map[string]interface{}{
			"mailbox":  mailboxKey,
			"folder":   folder,
			"from_uid": fromUID,
			"last_uid": lastUID,
			"uid_next": selected.UIDNext,
			"exists":   selected.NumMessages,
		}).Info("alias-index imap folder synced")
		return 0, lastUID, nil
	}

	stats, err := w.indexUIDRange(ctx, client, mailboxKey, folder, fromUID, toUID)
	if err != nil {
		return stats.indexed, lastUID, err
	}
	baselineDone := true
	if cursor != nil {
		baselineDone = cursor.BaselineDone
	}
	// toUID is a hard upper bound on every UID that can exist up to this
	// point (UIDNEXT-1), so the cursor advances to it even if some UIDs in
	// the range matched nothing (e.g. expunged before we ever fetched them).
	newLastUID := toUID
	if err := w.indexer.store.SetIMAPCursor(IMAPCursor{
		Mailbox:      mailboxKey,
		Folder:       folder,
		UIDValidity:  selected.UIDValidity,
		LastUID:      newLastUID,
		BaselineDone: baselineDone,
	}); err != nil {
		return stats.indexed, lastUID, err
	}
	fields := map[string]interface{}{
		"mailbox":  mailboxKey,
		"folder":   folder,
		"from_uid": fromUID,
		"to_uid":   toUID,
		"last_uid": newLastUID,
		"indexed":  stats.indexed,
		"uid_next": selected.UIDNext,
	}
	if stats.indexed == 0 {
		fields["skipped_seen"] = stats.skippedSeen
		fields["skipped_other"] = stats.skippedOther
	}
	Log().WithFields(fields).Info("alias-index imap folder synced")
	return stats.indexed, newLastUID, nil
}

type imapIndexStats struct {
	indexed      int
	skippedSeen  int
	skippedOther int
}

// indexUIDRange fetches and indexes every message in [fromUID, toUID],
// chunked to SearchBatch-sized batches so a large backlog (e.g. after
// extended downtime) doesn't arrive as one enormous FETCH response.
//
// UIDs are listed out explicitly (1,2,3,...) rather than sent as a compact
// N:M range: some servers (observed against Tencent Exmail) return an empty
// HEADER.FIELDS body for a single-UID N:N range on a FETCH, even though the
// identical UID as a bare number or as part of a longer list works fine.
// Listing UIDs explicitly needs no extra round trip — we already know the
// exact range from UIDNEXT — and matches the request shape the baseline path
// has always used successfully.
func (w *IMAPAccountWatcher) indexUIDRange(ctx context.Context, client *imapMailboxClient, mailboxKey, folder string, fromUID, toUID uint32) (imapIndexStats, error) {
	var stats imapIndexStats
	batchSize := uint32(w.cfg.SearchBatch)
	for start := fromUID; start <= toUID; {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		end := start + batchSize - 1
		if end > toUID || end < start {
			end = toUID
		}
		batch := make([]uint32, 0, end-start+1)
		for uid := start; uid <= end; uid++ {
			batch = append(batch, uid)
		}
		messages, err := client.FetchHeaderFields(batch)
		if err != nil {
			return stats, fmt.Errorf("fetch headers: %w", err)
		}
		messages, err = w.retryEmptyHeaderFetches(ctx, client, mailboxKey, folder, messages)
		if err != nil {
			return stats, err
		}
		for _, msg := range messages {
			outcome, err := indexMessageFromHeaders(w.indexer.store, msg.Headers, indexMessageOpts{
				mailboxKey:  mailboxKey,
				mailboxUser: w.account.User,
				tenantID:    w.account.TenantID,
				since:       w.indexer.cfg.Since,
				folder:      folder,
			})
			if err != nil {
				return stats, err
			}
			switch outcome {
			case outcomeIndexed:
				stats.indexed++
			case outcomeSkippedSeen:
				stats.skippedSeen++
			default:
				stats.skippedOther++
			}
		}
		if end == toUID {
			break
		}
		start = end + 1
	}
	return stats, nil
}

// headerBlockLooksEmpty reports whether none of the fields that matter for
// indexing came back — the signature of a HEADER.FIELDS fetch that matched
// nothing, as opposed to a message that legitimately lacks Cc/Delivered-To.
func headerBlockLooksEmpty(h parsedMailHeaders) bool {
	return h.MessageID == "" && h.From == "" && h.To == ""
}

// retryEmptyHeaderFetches re-fetches any message whose header block came
// back empty. Freshly delivered mail can have its EXISTS/UIDNEXT already
// bumped while the message content isn't fully available yet for a
// HEADER.FIELDS fetch (observed against Tencent Exmail: brand-new mail
// consistently fetched with zero matching header fields, while the same
// fetch shape has always worked for older, already-settled mail). A short
// retry avoids permanently skipping mail that just needed a moment to settle
// server-side, at the cost of a few extra seconds only on the rare messages
// that hit this.
func (w *IMAPAccountWatcher) retryEmptyHeaderFetches(ctx context.Context, client *imapMailboxClient, mailboxKey, folder string, messages []imapFetchedMessage) ([]imapFetchedMessage, error) {
	pending := make([]uint32, 0)
	for _, msg := range messages {
		if headerBlockLooksEmpty(msg.Headers) {
			pending = append(pending, msg.UID)
		}
	}
	if len(pending) == 0 {
		return messages, nil
	}
	byUID := make(map[uint32]imapFetchedMessage, len(messages))
	for _, msg := range messages {
		byUID[msg.UID] = msg
	}

	for attempt := 1; attempt <= imapEmptyHeaderMaxRetries && len(pending) > 0; attempt++ {
		if !sleepContext(ctx, imapEmptyHeaderRetryDelay) {
			break
		}
		Log().WithFields(map[string]interface{}{
			"mailbox": mailboxKey,
			"folder":  folder,
			"uids":    pending,
			"attempt": attempt,
		}).Warn("alias-index imap retrying fetch for empty header block")

		refetched, err := client.FetchHeaderFields(pending)
		if err != nil {
			return nil, fmt.Errorf("retry fetch headers: %w", err)
		}
		refByUID := make(map[uint32]imapFetchedMessage, len(refetched))
		for _, msg := range refetched {
			refByUID[msg.UID] = msg
		}
		next := pending[:0]
		for _, uid := range pending {
			msg, ok := refByUID[uid]
			if !ok {
				// Message is gone (expunged between fetches); keep whatever
				// we had from the first attempt rather than looping on it.
				continue
			}
			if headerBlockLooksEmpty(msg.Headers) {
				next = append(next, uid)
				continue
			}
			byUID[uid] = msg
		}
		pending = next
	}

	out := make([]imapFetchedMessage, 0, len(byUID))
	for _, msg := range byUID {
		out = append(out, msg)
	}
	return out, nil
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
