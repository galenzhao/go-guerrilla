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
  uidl TEXT PRIMARY KEY,
  seen_at INTEGER NOT NULL
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
	return nil
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
	_, err := s.db.Exec(
		`INSERT INTO alias_threads (message_id, reply_as, orig_from, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(message_id) DO UPDATE SET
		   reply_as = excluded.reply_as,
		   orig_from = excluded.orig_from,
		   created_at = excluded.created_at`,
		messageID, replyAs, origFrom, createdAt.Unix(),
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
		`SELECT message_id, reply_as, orig_from, created_at FROM alias_threads WHERE message_id = ?`,
		messageID,
	)
	var (
		id, replyAs, origFrom string
		createdAtUnix         int64
	)
	if err := row.Scan(&id, &replyAs, &origFrom, &createdAtUnix); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &AliasThread{
		MessageID: id,
		ReplyAs:   replyAs,
		OrigFrom:  origFrom,
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

// IsKnownUIDL reports whether a POP3 UIDL was already seen.
func (s *AliasStore) IsKnownUIDL(uidl string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("alias store not open")
	}
	uidl = strings.TrimSpace(uidl)
	if uidl == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM alias_index_uidl WHERE uidl = ? LIMIT 1`, uidl).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MarkUIDLKnown records a POP3 UIDL without indexing message content.
func (s *AliasStore) MarkUIDLKnown(uidl string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alias store not open")
	}
	uidl = strings.TrimSpace(uidl)
	if uidl == "" {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO alias_index_uidl (uidl, seen_at) VALUES (?, ?)
		 ON CONFLICT(uidl) DO NOTHING`,
		uidl, time.Now().UTC().Unix(),
	)
	return err
}

// MarkUIDLsKnown records many UIDLs in one transaction.
func (s *AliasStore) MarkUIDLsKnown(uidls []string) error {
	if len(uidls) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO alias_index_uidl (uidl, seen_at) VALUES (?, ?) ON CONFLICT(uidl) DO NOTHING`)
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
		if _, err := stmt.Exec(uidl, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
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
