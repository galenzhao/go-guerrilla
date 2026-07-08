package backends

import (
	"path/filepath"
	"testing"
	"time"
)

func TestIndexMessageFromHeaders(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: filepath.Join(dir, "alias.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	headers := parsedMailHeaders{
		MessageID:   "<msg-1@example.com>",
		From:        "sender@qq.com",
		To:          "support@example.com",
		DeliveredTo: "catchall@example.com",
		Date:        time.Now().UTC(),
	}
	outcome, err := indexMessageFromHeaders(store, headers, indexMessageOpts{
		mailboxKey:  "imap:user@example.com@imap.example.com",
		mailboxUser: "catchall@example.com",
		tenantID:    "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != outcomeIndexed {
		t.Fatalf("expected indexed, got %d", outcome)
	}
	thread, err := store.LookupThread("<msg-1@example.com>")
	if err != nil || thread == nil || thread.ReplyAs != "support@example.com" {
		t.Fatalf("lookup failed: %+v err=%v", thread, err)
	}

	outcome, err = indexMessageFromHeaders(store, headers, indexMessageOpts{
		mailboxKey:  "imap:user@example.com@imap.example.com",
		mailboxUser: "catchall@example.com",
		tenantID:    "acme",
		folder:      "OtherFolder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != outcomeSkippedSeen {
		t.Fatalf("expected skipped seen on duplicate message-id across folders, got %d", outcome)
	}
}

func TestIndexMessageFromHeadersSkipsDate(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: filepath.Join(dir, "alias.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	headers := parsedMailHeaders{
		MessageID: "<old@example.com>",
		From:      "sender@qq.com",
		To:        "support@example.com",
		Date:      time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	outcome, err := indexMessageFromHeaders(store, headers, indexMessageOpts{
		mailboxKey:  "imap:user@example.com@imap.example.com",
		mailboxUser: "catchall@example.com",
		since:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != outcomeSkippedDate {
		t.Fatalf("expected skipped date, got %d", outcome)
	}
	seen, err := store.IsSeenMessage("imap:user@example.com@imap.example.com", "<old@example.com>")
	if err != nil || !seen {
		t.Fatalf("expected old message marked seen")
	}
}
