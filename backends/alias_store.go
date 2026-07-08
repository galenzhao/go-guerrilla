package backends

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultAliasIndexTTL       = 180 * 24 * time.Hour
	defaultAliasIndexMaxRows   = 100000
	defaultAliasIndexPurge     = time.Hour
	aliasIndexUIDLBatchSize    = 2000
	aliasThreadsSchema         = `
CREATE TABLE IF NOT EXISTS alias_threads (
  message_id TEXT PRIMARY KEY,
  reply_as   TEXT NOT NULL,
  orig_from  TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_alias_threads_created_at ON alias_threads(created_at);
`
	aliasIndexUIDLSchema = `
CREATE TABLE IF NOT EXISTS alias_index_uidl (
  mailbox TEXT NOT NULL DEFAULT '',
  uidl TEXT NOT NULL,
  seen_at INTEGER NOT NULL,
  PRIMARY KEY (mailbox, uidl)
);
`
	aliasIndexStateSchema = `
CREATE TABLE IF NOT EXISTS alias_index_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
)

// AliasThread is a single Message-ID to reply-as mapping.
type AliasThread struct {
	MessageID string
	ReplyAs   string
	OrigFrom  string
	TenantID  string
	CreatedAt time.Time
}

// AliasStoreConfig controls retention for alias_threads.
type AliasStoreConfig struct {
	DBPath          string
	TTL             time.Duration
	MaxRows         int
	PurgeInterval   time.Duration
}

// AliasStore persists alias thread metadata in SQLite.
type AliasStore struct {
	db     *sql.DB
	config AliasStoreConfig
}

// OpenAliasStore opens (or creates) the alias SQLite database.
func OpenAliasStore(cfg AliasStoreConfig) (*AliasStore, error) {
	path := strings.TrimSpace(cfg.DBPath)
	if path == "" {
		return nil, fmt.Errorf("alias_db_path is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultAliasIndexTTL
	}
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = defaultAliasIndexMaxRows
	}
	if cfg.PurgeInterval <= 0 {
		cfg.PurgeInterval = defaultAliasIndexPurge
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &AliasStore{db: db, config: cfg}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *AliasStore) migrate() error {
	for _, stmt := range []string{aliasThreadsSchema, aliasIndexUIDLSchema, aliasIndexStateSchema} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("alias store migrate: %w", err)
		}
	}
	if err := s.migrateUIDLTable(); err != nil {
		return err
	}
	return s.migrateTenantIDColumn()
}

func (s *AliasStore) migrateTenantIDColumn() error {
	var hasTenantID int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('alias_threads') WHERE name = 'tenant_id'`,
	).Scan(&hasTenantID)
	if err != nil {
		return err
	}
	if hasTenantID > 0 {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE alias_threads ADD COLUMN tenant_id TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return fmt.Errorf("alias store migrate tenant_id: %w", err)
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_alias_threads_tenant ON alias_threads(tenant_id)`)
	return err
}

func (s *AliasStore) migrateUIDLTable() error {
	var hasMailbox int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('alias_index_uidl') WHERE name = 'mailbox'`,
	).Scan(&hasMailbox)
	if err != nil {
		return err
	}
	if hasMailbox > 0 {
		return nil
	}
	// Upgrade legacy single-mailbox schema (uidl PRIMARY KEY only).
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS alias_index_uidl_v2 (
		  mailbox TEXT NOT NULL DEFAULT '',
		  uidl TEXT NOT NULL,
		  seen_at INTEGER NOT NULL,
		  PRIMARY KEY (mailbox, uidl)
		)`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO alias_index_uidl_v2 (mailbox, uidl, seen_at)
		SELECT '', uidl, seen_at FROM alias_index_uidl`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DROP TABLE alias_index_uidl`); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE alias_index_uidl_v2 RENAME TO alias_index_uidl`)
	return err
}

// Close closes the underlying database handle.
func (s *AliasStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// UpsertThread inserts or replaces a thread mapping.
func (s *AliasStore) UpsertThread(thread AliasThread) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alias store not open")
	}
	messageID := normalizeMessageID(thread.MessageID)
	if messageID == "" {
		return fmt.Errorf("empty message_id")
	}
	replyAs := normalizeEmailAddress(thread.ReplyAs)
	origFrom := normalizeEmailAddress(thread.OrigFrom)
	if replyAs == "" || origFrom == "" {
		return fmt.Errorf("reply_as and orig_from are required")
	}
	createdAt := thread.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	tenantID := strings.TrimSpace(thread.TenantID)
	_, err := s.db.Exec(
		`INSERT INTO alias_threads (message_id, reply_as, orig_from, tenant_id, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(message_id) DO UPDATE SET
		   reply_as = excluded.reply_as,
		   orig_from = excluded.orig_from,
		   tenant_id = excluded.tenant_id,
		   created_at = excluded.created_at`,
		messageID, replyAs, origFrom, tenantID, createdAt.Unix(),
	)
	return err
}

// LookupThread returns the mapping for a Message-ID, if present.
func (s *AliasStore) LookupThread(messageID string) (*AliasThread, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("alias store not open")
	}
	messageID = normalizeMessageID(messageID)
	if messageID == "" {
		return nil, nil
	}
	row := s.db.QueryRow(
		`SELECT message_id, reply_as, orig_from, tenant_id, created_at FROM alias_threads WHERE message_id = ?`,
		messageID,
	)
	var (
		id, replyAs, origFrom, tenantID string
		createdAtUnix                   int64
	)
	if err := row.Scan(&id, &replyAs, &origFrom, &tenantID, &createdAtUnix); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &AliasThread{
		MessageID: id,
		ReplyAs:   replyAs,
		OrigFrom:  origFrom,
		TenantID:  tenantID,
		CreatedAt: time.Unix(createdAtUnix, 0).UTC(),
	}, nil
}

// LookupAnyThread tries each Message-ID until one matches.
func (s *AliasStore) LookupAnyThread(messageIDs []string) (*AliasThread, string, error) {
	for _, raw := range messageIDs {
		thread, err := s.LookupThread(raw)
		if err != nil {
			return nil, "", err
		}
		if thread != nil {
			return thread, normalizeMessageID(raw), nil
		}
	}
	return nil, "", nil
}

// PurgeExpired deletes rows older than TTL and enforces max_rows.
func (s *AliasStore) PurgeExpired() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alias store not open")
	}
	cutoff := time.Now().UTC().Add(-s.config.TTL).Unix()
	if _, err := s.db.Exec(`DELETE FROM alias_threads WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM alias_threads`).Scan(&count); err != nil {
		return err
	}
	if count <= s.config.MaxRows {
		return nil
	}
	excess := count - s.config.MaxRows
	_, err := s.db.Exec(
		`DELETE FROM alias_threads WHERE message_id IN (
			SELECT message_id FROM alias_threads ORDER BY created_at ASC LIMIT ?
		)`,
		excess,
	)
	return err
}

// HasBaseline returns true when the POP3 UIDL baseline was recorded.
func (s *AliasStore) HasBaseline(key string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("alias store not open")
	}
	var value string
	err := s.db.QueryRow(`SELECT value FROM alias_index_state WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == "baseline", nil
}

// SetBaseline marks the POP3 UIDL baseline as recorded.
func (s *AliasStore) SetBaseline(key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alias store not open")
	}
	_, err := s.db.Exec(
		`INSERT INTO alias_index_state (key, value) VALUES (?, 'baseline')
		 ON CONFLICT(key) DO UPDATE SET value = 'baseline'`,
		key,
	)
	return err
}

// IsKnownUIDL reports whether a POP3 UIDL was already seen for a mailbox.
func (s *AliasStore) IsKnownUIDL(mailbox, uidl string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("alias store not open")
	}
	mailbox = strings.TrimSpace(mailbox)
	uidl = strings.TrimSpace(uidl)
	if uidl == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM alias_index_uidl WHERE mailbox = ? AND uidl = ? LIMIT 1`,
		mailbox, uidl,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MarkUIDLKnown records a POP3 UIDL without indexing message content.
func (s *AliasStore) MarkUIDLKnown(mailbox, uidl string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alias store not open")
	}
	mailbox = strings.TrimSpace(mailbox)
	uidl = strings.TrimSpace(uidl)
	if uidl == "" {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO alias_index_uidl (mailbox, uidl, seen_at) VALUES (?, ?, ?)
		 ON CONFLICT(mailbox, uidl) DO NOTHING`,
		mailbox, uidl, time.Now().UTC().Unix(),
	)
	return err
}

// MarkUIDLsKnown records many UIDLs in batched transactions for a mailbox.
func (s *AliasStore) MarkUIDLsKnown(mailbox string, uidls []string) error {
	if len(uidls) == 0 {
		return nil
	}
	mailbox = strings.TrimSpace(mailbox)
	for start := 0; start < len(uidls); start += aliasIndexUIDLBatchSize {
		end := start + aliasIndexUIDLBatchSize
		if end > len(uidls) {
			end = len(uidls)
		}
		if err := s.markUIDLsKnownBatch(mailbox, uidls[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *AliasStore) markUIDLsKnownBatch(mailbox string, uidls []string) error {
	if len(uidls) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO alias_index_uidl (mailbox, uidl, seen_at) VALUES (?, ?, ?) ON CONFLICT(mailbox, uidl) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC().Unix()
	for _, uidl := range uidls {
		uidl = strings.TrimSpace(uidl)
		if uidl == "" {
			continue
		}
		if _, err := stmt.Exec(mailbox, uidl, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// KnownUIDLsForMailbox returns all recorded UIDLs for a mailbox.
func (s *AliasStore) KnownUIDLsForMailbox(mailbox string) (map[string]struct{}, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("alias store not open")
	}
	mailbox = strings.TrimSpace(mailbox)
	rows, err := s.db.Query(`SELECT uidl FROM alias_index_uidl WHERE mailbox = ?`, mailbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := make(map[string]struct{})
	for rows.Next() {
		var uidl string
		if err := rows.Scan(&uidl); err != nil {
			return nil, err
		}
		known[uidl] = struct{}{}
	}
	return known, rows.Err()
}

// ParseAliasDuration parses config durations like "180d" or "1h".
func ParseAliasDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: %w", s, err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
