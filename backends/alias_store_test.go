package backends

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeEmailAddress(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"galenzhao1@icloud.com", "galenzhao1@icloud.com"},
		{"zhaozhenggang <galenzhao1@icloud.com>", "galenzhao1@icloud.com"},
		{"Support <support@example.com>", "support@example.com"},
	}
	for _, tc := range tests {
		if got := normalizeEmailAddress(tc.in); got != tc.want {
			t.Fatalf("normalizeEmailAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeMessageID(t *testing.T) {
	if got := normalizeMessageID("<Tencent@QQ.com>"); got != "Tencent@qq.com" {
		t.Fatalf("got %q", got)
	}
}

func TestParseMessageIDs(t *testing.T) {
	ids := parseMessageIDs("<a@example.com> <b@example.com>")
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %v", ids)
	}
}

func TestAliasStoreUpsertLookupPurge(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "alias.db")
	store, err := OpenAliasStore(AliasStoreConfig{
		DBPath:  dbPath,
		TTL:     time.Hour,
		MaxRows: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	thread := AliasThread{
		MessageID: "<msg-1@example.com>",
		ReplyAs:   "support@example.com",
		OrigFrom:  "sender@qq.com",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	if err := store.UpsertThread(thread); err != nil {
		t.Fatal(err)
	}
	got, err := store.LookupThread("msg-1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ReplyAs != "support@example.com" {
		t.Fatalf("lookup failed: %+v", got)
	}

	if err := store.PurgeExpired(); err != nil {
		t.Fatal(err)
	}
	got, err = store.LookupThread("msg-1@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected purge to remove old row, got %+v", got)
	}
}

func TestAliasStoreTenantID(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: filepath.Join(dir, "alias.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertThread(AliasThread{
		MessageID: "<msg-tenant@example.com>",
		ReplyAs:   "support@example.com",
		OrigFrom:  "sender@qq.com",
		TenantID:  "acme",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LookupThread("<msg-tenant@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.TenantID != "acme" {
		t.Fatalf("tenant_id not stored: %+v", got)
	}
}

func TestAliasStoreUIDLBaseline(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: filepath.Join(dir, "alias.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	key := "pop3:all@example.com"
	has, err := store.HasBaseline(key)
	if err != nil || has {
		t.Fatalf("unexpected baseline: has=%v err=%v", has, err)
	}
	if err := store.MarkUIDLsKnown(key, []string{"uid-1", "uid-2"}); err != nil {
		t.Fatal(err)
	}
	known, err := store.IsKnownUIDL(key, "uid-1")
	if err != nil || !known {
		t.Fatalf("uid-1 should be known")
	}
	if err := store.SetBaseline(key); err != nil {
		t.Fatal(err)
	}
	has, err = store.HasBaseline(key)
	if err != nil || !has {
		t.Fatalf("baseline not set")
	}
	_ = os.RemoveAll(dir)
}

func TestAliasStoreMarkUIDLsKnownBatched(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: filepath.Join(dir, "alias.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	key := "pop3:all@example.com"
	uidls := make([]string, aliasIndexUIDLBatchSize+50)
	for i := range uidls {
		uidls[i] = fmt.Sprintf("uid-%d", i)
	}
	if err := store.MarkUIDLsKnown(key, uidls); err != nil {
		t.Fatal(err)
	}
	known, err := store.KnownUIDLsForMailbox(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != len(uidls) {
		t.Fatalf("expected %d known uidls, got %d", len(uidls), len(known))
	}
}

func TestAliasStoreKnownUIDLsForMailbox(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: filepath.Join(dir, "alias.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	key := "pop3:user@example.com"
	if err := store.MarkUIDLsKnown(key, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	known, err := store.KnownUIDLsForMailbox(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known["a"]; !ok || len(known) != 2 {
		t.Fatalf("unexpected known set: %+v", known)
	}
}
