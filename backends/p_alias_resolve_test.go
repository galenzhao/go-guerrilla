package backends

import (
	"context"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
)

func TestAliasResolveInjectTenantSend(t *testing.T) {
	defer Svc.reset()
	defer SetGlobalTenantRegistry(nil)

	dir := t.TempDir()
	dbPath := dir + "/alias.db"
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertThread(AliasThread{
		MessageID: "<tencent_456@qq.com>",
		ReplyAs:   "support@example.com",
		OrigFrom:  "galenzha0@vip.qq.com",
		TenantID:  "acme",
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reg := &stubTenantRegistry{tenants: map[string]Tenant{
		"acme": {
			TenantID: "acme",
			Provider: "oci",
			OCIEmail: &TenantOCI{Region: "us-phoenix-1", Username: "u", Password: "p"},
		},
	}}
	SetGlobalTenantRegistry(reg)

	deco := AliasResolve()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))

	if err := Svc.initialize(BackendConfig{"alias_db_path": dbPath, "alias_fail_hard": true}); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.Data.WriteString("In-Reply-To: <tencent_456@qq.com>\r\n\r\nBody\r\n")
	_ = e.ParseHeaders()

	if _, err := p.Process(e, TaskSaveMail); err != nil {
		t.Fatal(err)
	}
	cfg := GetEnvelopeTenantSend(e)
	if cfg == nil || cfg.TenantID != "acme" {
		t.Fatalf("expected tenant_send on envelope, got %+v", cfg)
	}
}

func TestAliasResolveRequiresTenantWhenRegistryConfigured(t *testing.T) {
	defer Svc.reset()
	defer SetGlobalTenantRegistry(nil)

	dir := t.TempDir()
	dbPath := dir + "/alias.db"
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertThread(AliasThread{
		MessageID: "<no-tenant@qq.com>",
		ReplyAs:   "support@example.com",
		OrigFrom:  "galenzha0@vip.qq.com",
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	SetGlobalTenantRegistry(&stubTenantRegistry{tenants: map[string]Tenant{}})

	deco := AliasResolve()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))
	if err := Svc.initialize(BackendConfig{"alias_db_path": dbPath, "alias_fail_hard": true}); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.Data.WriteString("In-Reply-To: <no-tenant@qq.com>\r\n\r\nBody\r\n")
	_ = e.ParseHeaders()

	res, err := p.Process(e, TaskSaveMail)
	if err == nil {
		t.Fatal("expected error when tenant_id missing under tenant_registry")
	}
	if res.Code() != 554 {
		t.Fatalf("expected 554, got %d (%s)", res.Code(), res.String())
	}
}

type stubTenantRegistry struct {
	tenants map[string]Tenant
}

func (s *stubTenantRegistry) Refresh(_ context.Context) error { return nil }
func (s *stubTenantRegistry) All() []Tenant {
	out := make([]Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		out = append(out, t)
	}
	return out
}
func (s *stubTenantRegistry) Get(tenantID string) (*Tenant, bool) {
	t, ok := s.tenants[tenantID]
	if !ok {
		return nil, false
	}
	copy := t
	return &copy, true
}
func (s *stubTenantRegistry) POP3Accounts() []TaggedPOP3Account { return nil }
func (s *stubTenantRegistry) PollInterval() time.Duration       { return time.Minute }

func TestAliasResolveInjectReplyAs(t *testing.T) {
	defer Svc.reset()

	dir := t.TempDir()
	dbPath := dir + "/alias.db"
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertThread(AliasThread{
		MessageID: "<tencent_123@qq.com>",
		ReplyAs:   "support@example.com",
		OrigFrom:  "galenzha0@vip.qq.com",
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	deco := AliasResolve()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))

	backendCfg := BackendConfig{
		"alias_db_path":   dbPath,
		"alias_fail_hard": true,
	}
	if err := Svc.initialize(backendCfg); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.RcptTo = []mail.Address{{User: "galenzha0", Host: "vip.qq.com"}}
	e.Data.WriteString("From: me <me@icloud.com>\r\n")
	e.Data.WriteString("To: Galen <galenzha0@vip.qq.com>\r\n")
	e.Data.WriteString("In-Reply-To: <tencent_123@qq.com>\r\n")
	e.Data.WriteString("\r\nBody\r\n")
	_ = e.ParseHeaders()

	_, err = p.Process(e, TaskSaveMail)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Header.Get(aliasReplyAsHeader); got != "support@example.com" {
		t.Fatalf("expected reply-as header, got %q", got)
	}
}

func TestAliasResolveFailHardOnMiss(t *testing.T) {
	defer Svc.reset()

	dir := t.TempDir()
	dbPath := dir + "/alias.db"
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	deco := AliasResolve()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))

	backendCfg := BackendConfig{
		"alias_db_path":   dbPath,
		"alias_fail_hard": true,
	}
	if err := Svc.initialize(backendCfg); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.Data.WriteString("In-Reply-To: <missing@example.com>\r\n\r\nBody\r\n")
	_ = e.ParseHeaders()

	res, err := p.Process(e, TaskSaveMail)
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Code() != 554 {
		t.Fatalf("expected 554, got %d (%s)", res.Code(), res.String())
	}
	if !strings.Contains(res.String(), "alias not found") {
		t.Fatalf("unexpected response: %s", res.String())
	}
}

func TestAliasResolveSkipsNonReply(t *testing.T) {
	defer Svc.reset()

	dir := t.TempDir()
	dbPath := dir + "/alias.db"
	store, err := OpenAliasStore(AliasStoreConfig{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	deco := AliasResolve()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))

	if err := Svc.initialize(BackendConfig{"alias_db_path": dbPath}); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.Data.WriteString("From: me@icloud.com\r\nTo: a@example.com\r\n\r\nBody\r\n")
	_ = e.ParseHeaders()

	_, err = p.Process(e, TaskSaveMail)
	if err != nil {
		t.Fatal(err)
	}
	if e.Header == nil {
		e.Header = textproto.MIMEHeader{}
	}
	if got := e.Header.Get(aliasReplyAsHeader); got != "" {
		t.Fatalf("expected no reply-as header, got %q", got)
	}
}

func TestParseMailHeaders(t *testing.T) {
	raw := "Message-Id: <abc@example.com>\r\nFrom: Sender <sender@qq.com>\r\nTo: support@example.com\r\n\r\nbody"
	h, err := parseMailHeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.MessageID != "<abc@example.com>" {
		t.Fatalf("message-id: %q", h.MessageID)
	}
	if extractFirstEmailFromHeader(h.To) != "support@example.com" {
		t.Fatalf("to: %q", h.To)
	}
}
