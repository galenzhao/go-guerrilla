package backends

import (
	"strings"
	"testing"

	"github.com/flashmob/go-guerrilla/mail"
)

func TestRewriteFromHeader_ReplacesExisting(t *testing.T) {
	raw := []byte("From: Old Name <old@example.com>\r\nTo: a@example.com\r\n\r\nBody")
	out, changed, err := rewriteFromHeader(raw, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	s := string(out)
	if !strings.Contains(s, "From: new@example.com\r\n") {
		t.Fatalf("expected From rewritten, got: %q", s)
	}
	if !strings.Contains(s, "\r\n\r\nBody") {
		t.Fatalf("expected body preserved, got: %q", s)
	}
}

func TestRewriteFromHeader_InsertsIfMissing(t *testing.T) {
	raw := []byte("To: a@example.com\nSubject: x\n\nBody")
	out, changed, err := rewriteFromHeader(raw, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	s := string(out)
	if !strings.HasPrefix(s, "From: new@example.com\n") {
		t.Fatalf("expected From inserted at top, got: %q", s)
	}
}

func TestRewriteFromProcessor_RewritesEnvelopeData(t *testing.T) {
	defer Svc.reset()

	// build a small pipeline: RewriteFrom only (next is Noop)
	deco := RewriteFrom()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))

	backendCfg := BackendConfig{
		"ses_source_tmpl": "sender@example.com",
	}
	if err := Svc.initialize(backendCfg); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.Data.WriteString("From: Old <old@example.com>\nTo: a@example.com\n\nBody")

	_, err := p.Process(e, TaskSaveMail)
	if err != nil {
		t.Fatal(err)
	}
	got := e.Data.String()
	if !strings.HasPrefix(got, "From: sender@example.com\n") {
		t.Fatalf("expected rewritten From, got: %q", got)
	}
}

func TestRewriteFromProcessor_PreservesAliasReplyHeader(t *testing.T) {
	defer Svc.reset()

	deco := RewriteFrom()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		vars := makeSESVars(e)
		if vars["hdr_x_guerrilla_reply_as"] != "support@example.com" {
			t.Fatalf("expected preserved reply-as in template vars, got %q", vars["hdr_x_guerrilla_reply_as"])
		}
		return NewResult("250 OK"), nil
	}))

	if err := Svc.initialize(BackendConfig{"ociemail_source_tmpl": "${hdr_x_guerrilla_reply_as}"}); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.Data.WriteString("From: Old <old@example.com>\nTo: a@example.com\n\nBody")
	_ = e.ParseHeaders()
	e.Header.Set(aliasReplyAsHeader, "support@example.com")
	e.Values = map[string]interface{}{"alias_reply_as": "support@example.com"}

	_, err := p.Process(e, TaskSaveMail)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Header.Get(aliasReplyAsHeader); got != "support@example.com" {
		t.Fatalf("expected preserved header, got %q", got)
	}
}
