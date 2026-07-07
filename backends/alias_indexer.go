package backends

import (
	"bufio"
	"bytes"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// parsedMailHeaders holds fields extracted from a message header block.
type parsedMailHeaders struct {
	MessageID    string
	From         string
	To           string
	Cc           string
	DeliveredTo  string
	XOriginalTo  string
	Date         time.Time
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

// AliasIndexerConfig configures the POP3 alias indexer.
type AliasIndexerConfig struct {
	DBPath                  string
	POP3Host                string
	POP3Port                int
	POP3TLS                 bool
	POP3User                string
	POP3Password            string
	PollInterval            time.Duration
	PurgeInterval           time.Duration
	SkipExistingOnStart     bool
	Since                   time.Time
	StoreCfg                AliasStoreConfig
}

// AliasIndexer polls a POP3 mailbox and writes alias thread mappings.
type AliasIndexer struct {
	cfg   AliasIndexerConfig
	store *AliasStore
}

// NewAliasIndexer creates an indexer using the given config.
func NewAliasIndexer(cfg AliasIndexerConfig) (*AliasIndexer, error) {
	if strings.TrimSpace(cfg.POP3Host) == "" {
		return nil, fmt.Errorf("alias_index_pop3_host is required")
	}
	if strings.TrimSpace(cfg.POP3User) == "" {
		return nil, fmt.Errorf("alias_index_pop3_user is required")
	}
	if cfg.POP3Port <= 0 {
		cfg.POP3Port = 995
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

func (i *AliasIndexer) baselineKey() string {
	return fmt.Sprintf("pop3:%s@%s", i.cfg.POP3User, i.cfg.POP3Host)
}

// Run polls until the done channel is closed.
func (i *AliasIndexer) Run(done <-chan struct{}) error {
	purgeTicker := time.NewTicker(i.cfg.PurgeInterval)
	defer purgeTicker.Stop()
	pollTicker := time.NewTicker(i.cfg.PollInterval)
	defer pollTicker.Stop()

	if err := i.pollOnce(); err != nil {
		Log().WithError(err).Error("alias-index initial poll failed")
	}
	for {
		select {
		case <-done:
			return nil
		case <-purgeTicker.C:
			if err := i.store.PurgeExpired(); err != nil {
				Log().WithError(err).Warn("alias-index purge failed")
			}
		case <-pollTicker.C:
			if err := i.pollOnce(); err != nil {
				Log().WithError(err).Error("alias-index poll failed")
			}
		}
	}
}

func (i *AliasIndexer) pollOnce() error {
	client, err := dialPOP3(i.cfg.POP3Host, i.cfg.POP3Port, i.cfg.POP3TLS)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Auth(i.cfg.POP3User, i.cfg.POP3Password); err != nil {
		return err
	}

	uidls, err := client.UIDL()
	if err != nil {
		return err
	}

	baselineKey := i.baselineKey()
	hasBaseline, err := i.store.HasBaseline(baselineKey)
	if err != nil {
		return err
	}

	if i.cfg.SkipExistingOnStart && !hasBaseline {
		known := make([]string, 0, len(uidls))
		for _, item := range uidls {
			known = append(known, item.UIDL)
		}
		if err := i.store.MarkUIDLsKnown(known); err != nil {
			return err
		}
		if err := i.store.SetBaseline(baselineKey); err != nil {
			return err
		}
		Log().WithField("uidl_count", len(known)).Info("alias-index recorded POP3 UIDL baseline; existing mail skipped")
		return nil
	}

	indexed := 0
	skipped := 0
	for _, item := range uidls {
		known, err := i.store.IsKnownUIDL(item.UIDL)
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
			Log().WithError(err).WithField("uidl", item.UIDL).Warn("alias-index header parse failed; marking UIDL known")
			if err := i.store.MarkUIDLKnown(item.UIDL); err != nil {
				return err
			}
			continue
		}

		if !i.cfg.Since.IsZero() && !headers.Date.IsZero() && headers.Date.Before(i.cfg.Since) {
			if err := i.store.MarkUIDLKnown(item.UIDL); err != nil {
				return err
			}
			skipped++
			continue
		}

		messageID := normalizeMessageID(headers.MessageID)
		replyAs := extractReplyAsAddress(headers, i.cfg.POP3User)
		origFrom := extractFirstEmailFromHeader(headers.From)
		if messageID == "" || replyAs == "" || origFrom == "" {
			Log().WithFields(map[string]interface{}{
				"uidl":       item.UIDL,
				"message_id": headers.MessageID,
				"to":         headers.To,
				"delivered":  headers.DeliveredTo,
				"x_orig":     headers.XOriginalTo,
				"from":       headers.From,
			}).Debug("alias-index skipping message without indexable headers")
			if err := i.store.MarkUIDLKnown(item.UIDL); err != nil {
				return err
			}
			continue
		}

		if err := i.store.UpsertThread(AliasThread{
			MessageID: messageID,
			ReplyAs:   replyAs,
			OrigFrom:  origFrom,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := i.store.MarkUIDLKnown(item.UIDL); err != nil {
			return err
		}
		indexed++
	}

	if indexed > 0 || skipped > 0 {
		Log().WithFields(map[string]interface{}{
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
		POP3TLS:             true,
		POP3Port:            995,
	}
	if v, ok := backendConfig["alias_db_path"].(string); ok {
		cfg.DBPath = strings.TrimSpace(v)
	}
	if v, ok := backendConfig["alias_index_pop3_host"].(string); ok {
		cfg.POP3Host = strings.TrimSpace(v)
	}
	if v, ok := backendConfig["alias_index_pop3_user"].(string); ok {
		cfg.POP3User = strings.TrimSpace(v)
	}
	if v, ok := backendConfig["alias_index_pop3_password"].(string); ok {
		cfg.POP3Password = v
	}
	if v, ok := backendConfig["alias_index_pop3_port"].(float64); ok && int(v) > 0 {
		cfg.POP3Port = int(v)
	}
	if v, ok := backendConfig["alias_index_pop3_tls"].(bool); ok {
		cfg.POP3TLS = v
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
	cfg.StoreCfg.DBPath = cfg.DBPath
	return cfg, nil
}
