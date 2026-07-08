package backends

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"
)

// parsedMailHeaders holds fields extracted from a message header block.
type parsedMailHeaders struct {
	MessageID   string
	From        string
	To          string
	Cc          string
	DeliveredTo string
	XOriginalTo string
	Date        time.Time
}

func parseMailHeaders(raw string) (parsedMailHeaders, error) {
	var out parsedMailHeaders
	headerBytes := []byte(raw)
	if idx := bytes.Index(headerBytes, []byte("\r\n\r\n")); idx >= 0 {
		headerBytes = headerBytes[:idx+4]
	} else if idx := bytes.Index(headerBytes, []byte("\n\n")); idx >= 0 {
		headerBytes = headerBytes[:idx+2]
	}
	msg, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(headerBytes)))
	if err != nil {
		return out, err
	}
	out.MessageID = strings.TrimSpace(msg.Header.Get("Message-Id"))
	out.From = strings.TrimSpace(msg.Header.Get("From"))
	out.To = strings.TrimSpace(msg.Header.Get("To"))
	out.Cc = strings.TrimSpace(msg.Header.Get("Cc"))
	out.DeliveredTo = strings.TrimSpace(msg.Header.Get("Delivered-To"))
	out.XOriginalTo = strings.TrimSpace(msg.Header.Get("X-Original-To"))
	if dateHeader := strings.TrimSpace(msg.Header.Get("Date")); dateHeader != "" {
		if t, err := mail.ParseDate(dateHeader); err == nil {
			out.Date = t.UTC()
		}
	}
	return out, nil
}

// extractReplyAsAddress picks the alias address to reply as, skipping the mailbox itself.
func extractReplyAsAddress(h parsedMailHeaders, mailbox string) string {
	mailbox = normalizeEmailAddress(mailbox)
	skip := map[string]struct{}{mailbox: {}}
	if mailbox != "" {
		if at := strings.LastIndex(mailbox, "@"); at > 0 {
			skip["postmaster"+mailbox[at:]] = struct{}{}
		}
	}
	candidates := []string{h.To, h.Cc, h.DeliveredTo, h.XOriginalTo}
	for _, headerValue := range candidates {
		for _, part := range splitAddresses(headerValue) {
			addr := normalizeEmailAddress(part)
			if addr == "" {
				continue
			}
			if _, ok := skip[addr]; ok {
				continue
			}
			if strings.HasPrefix(addr, "null@") || strings.HasPrefix(addr, "noreply@") {
				continue
			}
			return addr
		}
	}
	return ""
}

// AliasPOP3Account is one POP3 mailbox to index.
type AliasPOP3Account struct {
	Host     string
	Port     int
	TLS      bool
	User     string
	Password string
	TenantID string
}

// MailboxKey returns the stable key for UIDL baseline and cursor storage.
func (a AliasPOP3Account) MailboxKey() string {
	return fmt.Sprintf("pop3:%s@%s", strings.TrimSpace(a.User), strings.TrimSpace(a.Host))
}

func (a AliasPOP3Account) validate() error {
	if strings.TrimSpace(a.Host) == "" {
		return fmt.Errorf("alias_index pop3 host is required")
	}
	if strings.TrimSpace(a.User) == "" {
		return fmt.Errorf("alias_index pop3 user is required for host %s", a.Host)
	}
	return nil
}

// AliasIndexerConfig configures the POP3 alias indexer.
type AliasIndexerConfig struct {
	DBPath              string
	Accounts            []AliasPOP3Account
	PollInterval        time.Duration
	PurgeInterval       time.Duration
	SkipExistingOnStart bool
	Since               time.Time
	StoreCfg            AliasStoreConfig
	Registry            TenantRegistry
}

// AliasIndexer polls one or more POP3 mailboxes and writes alias thread mappings.
type AliasIndexer struct {
	cfg   AliasIndexerConfig
	store *AliasStore
}

// NewAliasIndexer creates an indexer using the given config.
func NewAliasIndexer(cfg AliasIndexerConfig) (*AliasIndexer, error) {
	if cfg.Registry != nil {
		// Registry mode: ignore static Accounts; POP3 list comes only from GET /tenants.
		cfg.Accounts = nil
	} else if len(cfg.Accounts) == 0 {
		return nil, fmt.Errorf("at least one alias_index POP3 account or tenant_registry is required")
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Port <= 0 {
			cfg.Accounts[i].Port = 995
		}
		if err := cfg.Accounts[i].validate(); err != nil {
			return nil, fmt.Errorf("alias_index_pop3_accounts[%d]: %w", i, err)
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 60 * time.Second
	}
	if cfg.PurgeInterval <= 0 {
		cfg.PurgeInterval = defaultAliasIndexPurge
	}
	cfg.StoreCfg.DBPath = strings.TrimSpace(cfg.DBPath)
	if cfg.StoreCfg.DBPath == "" {
		return nil, fmt.Errorf("alias_db_path is required")
	}
	if cfg.StoreCfg.TTL <= 0 {
		cfg.StoreCfg.TTL = defaultAliasIndexTTL
	}
	if cfg.StoreCfg.MaxRows <= 0 {
		cfg.StoreCfg.MaxRows = defaultAliasIndexMaxRows
	}
	store, err := OpenAliasStore(cfg.StoreCfg)
	if err != nil {
		return nil, err
	}
	return &AliasIndexer{cfg: cfg, store: store}, nil
}

// Close closes the underlying store.
func (i *AliasIndexer) Close() error {
	if i == nil || i.store == nil {
		return nil
	}
	return i.store.Close()
}

// Run polls until the done channel is closed.
func (i *AliasIndexer) Run(done <-chan struct{}) error {
	purgeTicker := time.NewTicker(i.cfg.PurgeInterval)
	defer purgeTicker.Stop()
	pollTicker := time.NewTicker(i.cfg.PollInterval)
	defer pollTicker.Stop()

	var registryTicker *time.Ticker
	var registryC <-chan time.Time
	if i.cfg.Registry != nil {
		if interval := i.cfg.Registry.PollInterval(); interval > 0 {
			registryTicker = time.NewTicker(interval)
			defer registryTicker.Stop()
			registryC = registryTicker.C
			if err := i.cfg.Registry.Refresh(context.Background()); err != nil {
				Log().WithError(err).Warn("alias-index tenant registry refresh failed on start")
			} else {
				i.refreshAccountsFromRegistry()
			}
		}
	}

	i.pollAll()
	for {
		select {
		case <-done:
			return nil
		case <-purgeTicker.C:
			if err := i.store.PurgeExpired(); err != nil {
				Log().WithError(err).Warn("alias-index purge failed")
			}
		case <-registryC:
			if err := i.cfg.Registry.Refresh(context.Background()); err != nil {
				Log().WithError(err).Warn("alias-index tenant registry refresh failed")
			} else {
				i.refreshAccountsFromRegistry()
			}
		case <-pollTicker.C:
			i.pollAll()
		}
	}
}

func (i *AliasIndexer) refreshAccountsFromRegistry() {
	if i.cfg.Registry == nil {
		return
	}
	tagged := i.cfg.Registry.POP3Accounts()
	accounts := make([]AliasPOP3Account, 0, len(tagged))
	for _, item := range tagged {
		accounts = append(accounts, item.Account)
	}
	i.cfg.Accounts = accounts
	Log().WithField("mailboxes", len(accounts)).Info("alias-index refreshed POP3 accounts from tenant registry")
}

func (i *AliasIndexer) currentAccounts() []AliasPOP3Account {
	if i.cfg.Registry != nil {
		// Registry mode never falls back to static local POP3 accounts.
		tagged := i.cfg.Registry.POP3Accounts()
		accounts := make([]AliasPOP3Account, 0, len(tagged))
		for _, item := range tagged {
			accounts = append(accounts, item.Account)
		}
		return accounts
	}
	return i.cfg.Accounts
}

func (i *AliasIndexer) pollAll() {
	accounts := i.currentAccounts()
	if len(accounts) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, account := range accounts {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := i.pollMailbox(account); err != nil {
				Log().WithError(err).WithField("mailbox", account.MailboxKey()).Error("alias-index poll failed")
			}
		}()
	}
	wg.Wait()
}

func (i *AliasIndexer) pollMailbox(account AliasPOP3Account) error {
	client, err := dialPOP3(account.Host, account.Port, account.TLS)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Auth(account.User, account.Password); err != nil {
		return err
	}

	uidls, err := client.UIDL()
	if err != nil {
		return err
	}

	mailboxKey := account.MailboxKey()
	hasBaseline, err := i.store.HasBaseline(mailboxKey)
	if err != nil {
		return err
	}

	if i.cfg.SkipExistingOnStart && !hasBaseline {
		known := make([]string, 0, len(uidls))
		for _, item := range uidls {
			known = append(known, item.UIDL)
		}
		if err := i.store.MarkUIDLsKnown(mailboxKey, known); err != nil {
			return err
		}
		if err := i.store.SetBaseline(mailboxKey); err != nil {
			return err
		}
		Log().WithFields(map[string]interface{}{
			"mailbox":   mailboxKey,
			"uidl_count": len(known),
		}).Info("alias-index recorded POP3 UIDL baseline; existing mail skipped")
		return nil
	}

	indexed := 0
	skipped := 0
	for _, item := range uidls {
		known, err := i.store.IsKnownUIDL(mailboxKey, item.UIDL)
		if err != nil {
			return err
		}
		if known {
			continue
		}

		raw, err := client.Retr(item.Number)
		if err != nil {
			return err
		}
		headers, err := parseMailHeaders(raw)
		if err != nil {
			Log().WithError(err).WithFields(map[string]interface{}{
				"mailbox": mailboxKey,
				"uidl":    item.UIDL,
			}).Warn("alias-index header parse failed; marking UIDL known")
			if err := i.store.MarkUIDLKnown(mailboxKey, item.UIDL); err != nil {
				return err
			}
			continue
		}

		if !i.cfg.Since.IsZero() && !headers.Date.IsZero() && headers.Date.Before(i.cfg.Since) {
			if err := i.store.MarkUIDLKnown(mailboxKey, item.UIDL); err != nil {
				return err
			}
			skipped++
			continue
		}

		messageID := normalizeMessageID(headers.MessageID)
		replyAs := extractReplyAsAddress(headers, account.User)
		origFrom := extractFirstEmailFromHeader(headers.From)
		if messageID == "" || replyAs == "" || origFrom == "" {
			Log().WithFields(map[string]interface{}{
				"mailbox":    mailboxKey,
				"uidl":       item.UIDL,
				"message_id": headers.MessageID,
				"to":         headers.To,
				"delivered":  headers.DeliveredTo,
				"x_orig":     headers.XOriginalTo,
				"from":       headers.From,
			}).Debug("alias-index skipping message without indexable headers")
			if err := i.store.MarkUIDLKnown(mailboxKey, item.UIDL); err != nil {
				return err
			}
			continue
		}

		if err := i.store.UpsertThread(AliasThread{
			MessageID: messageID,
			ReplyAs:   replyAs,
			OrigFrom:  origFrom,
			TenantID:  account.TenantID,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := i.store.MarkUIDLKnown(mailboxKey, item.UIDL); err != nil {
			return err
		}
		indexed++
	}

	if indexed > 0 || skipped > 0 {
		Log().WithFields(map[string]interface{}{
			"mailbox": mailboxKey,
			"indexed": indexed,
			"skipped": skipped,
		}).Info("alias-index poll complete")
	}
	return nil
}

// AliasIndexerConfigFromBackend builds indexer config from backend_config map.
func AliasIndexerConfigFromBackend(backendConfig BackendConfig) (AliasIndexerConfig, error) {
	cfg := AliasIndexerConfig{
		SkipExistingOnStart: true,
	}
	if v, ok := backendConfig["alias_db_path"].(string); ok {
		cfg.DBPath = strings.TrimSpace(v)
	}
	if v, ok := backendConfig["alias_index_skip_existing_on_start"].(bool); ok {
		cfg.SkipExistingOnStart = v
	}
	if raw, ok := backendConfig["alias_index_poll_interval"].(string); ok && strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("invalid alias_index_poll_interval: %w", err)
		}
		cfg.PollInterval = d
	}
	if raw, ok := backendConfig["alias_index_purge_interval"].(string); ok && strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("invalid alias_index_purge_interval: %w", err)
		}
		cfg.PurgeInterval = d
	}
	if raw, ok := backendConfig["alias_index_ttl"].(string); ok && strings.TrimSpace(raw) != "" {
		d, err := ParseAliasDuration(raw)
		if err != nil {
			return cfg, fmt.Errorf("invalid alias_index_ttl: %w", err)
		}
		cfg.StoreCfg.TTL = d
	}
	if v, ok := backendConfig["alias_index_max_rows"].(float64); ok && int(v) > 0 {
		cfg.StoreCfg.MaxRows = int(v)
	}
	if raw, ok := backendConfig["alias_index_since"].(string); ok && strings.TrimSpace(raw) != "" {
		t, err := time.Parse("2006-01-02", strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("invalid alias_index_since: %w", err)
		}
		cfg.Since = t.UTC()
	}

	accounts, err := parseAliasPOP3Accounts(backendConfig)
	if err != nil {
		// Static POP3 accounts are optional when tenant_registry will supply them.
		if !strings.Contains(err.Error(), "is required") {
			return cfg, err
		}
	} else {
		cfg.Accounts = accounts
	}
	cfg.StoreCfg.DBPath = cfg.DBPath
	return cfg, nil
}

func parseAliasPOP3Accounts(backendConfig BackendConfig) ([]AliasPOP3Account, error) {
	if raw, ok := backendConfig["alias_index_pop3_accounts"]; ok && raw != nil {
		accounts, err := parseAliasPOP3AccountsList(raw)
		if err != nil {
			return nil, err
		}
		if len(accounts) > 0 {
			return accounts, nil
		}
	}
	legacy, err := parseLegacyAliasPOP3Account(backendConfig)
	if err != nil {
		return nil, err
	}
	if legacy != nil {
		return []AliasPOP3Account{*legacy}, nil
	}
	return nil, fmt.Errorf("alias_index_pop3_accounts or alias_index_pop3_host/user is required")
}

func parseAliasPOP3AccountsList(raw interface{}) ([]AliasPOP3Account, error) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("alias_index_pop3_accounts must be an array")
	}
	accounts := make([]AliasPOP3Account, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("alias_index_pop3_accounts[%d] must be an object", i)
		}
		account, err := parseAliasPOP3AccountMap(m)
		if err != nil {
			return nil, fmt.Errorf("alias_index_pop3_accounts[%d]: %w", i, err)
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func parseLegacyAliasPOP3Account(backendConfig BackendConfig) (*AliasPOP3Account, error) {
	host, _ := backendConfig["alias_index_pop3_host"].(string)
	user, _ := backendConfig["alias_index_pop3_user"].(string)
	if strings.TrimSpace(host) == "" && strings.TrimSpace(user) == "" {
		return nil, nil
	}
	account := AliasPOP3Account{
		Host:     strings.TrimSpace(host),
		User:     strings.TrimSpace(user),
		TLS:      true,
		Port:     995,
	}
	if v, ok := backendConfig["alias_index_pop3_password"].(string); ok {
		account.Password = v
	}
	if v, ok := backendConfig["alias_index_pop3_port"].(float64); ok && int(v) > 0 {
		account.Port = int(v)
	}
	if v, ok := backendConfig["alias_index_pop3_tls"].(bool); ok {
		account.TLS = v
	}
	return &account, nil
}

func parseAliasPOP3AccountMap(m map[string]interface{}) (AliasPOP3Account, error) {
	account := AliasPOP3Account{
		TLS:  true,
		Port: 995,
	}
	if v, ok := m["host"].(string); ok {
		account.Host = strings.TrimSpace(v)
	}
	if v, ok := m["user"].(string); ok {
		account.User = strings.TrimSpace(v)
	}
	if v, ok := m["password"].(string); ok {
		account.Password = v
	}
	if v, ok := m["port"].(float64); ok && int(v) > 0 {
		account.Port = int(v)
	}
	if v, ok := m["tls"].(bool); ok {
		account.TLS = v
	}
	return account, account.validate()
}
