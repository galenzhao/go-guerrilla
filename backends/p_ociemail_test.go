package backends

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/flashmob/go-guerrilla/log"
	"github.com/flashmob/go-guerrilla/mail"
)

type fakeOCIEmailSender struct {
	lastReq *ociemailSendRequest
	err     error
}

func (f *fakeOCIEmailSender) Send(_ context.Context, _ *ociemailConfig, req *ociemailSendRequest) error {
	f.lastReq = req
	if f.err != nil {
		return f.err
	}
	return nil
}

func TestResolveOCIEmailHost(t *testing.T) {
	host, err := resolveOCIEmailHost(&ociemailConfig{Host: "smtp.custom.example"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "smtp.custom.example" {
		t.Fatalf("unexpected host: %q", host)
	}

	host, err = resolveOCIEmailHost(&ociemailConfig{Region: "us-ashburn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if host != "smtp.email.us-ashburn-1.oci.oraclecloud.com" {
		t.Fatalf("unexpected host: %q", host)
	}

	if _, err := resolveOCIEmailHost(&ociemailConfig{}); err == nil {
		t.Fatal("expected error when host and region are empty")
	}
}

func TestOCIEmailProcessorSendTemplates(t *testing.T) {
	origNewSender := newOCIEmailSender
	defer func() { newOCIEmailSender = origNewSender }()

	fake := &fakeOCIEmailSender{}
	newOCIEmailSender = func(_ *ociemailConfig) ociemailSenderAPI {
		return fake
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.MailFrom = mail.Address{User: "alice", Host: "from.example"}
	e.RcptTo = append(e.RcptTo, mail.Address{User: "bob", Host: "rcpt.example"})
	_, _ = e.Data.WriteString("From: support@domain.example\r\nSubject: Hello\r\n\r\nBody")

	l, _ := log.GetLogger("./test_ociemail.log", "debug")
	g, err := New(BackendConfig{
		"save_process":                    "HeadersParser|Header|OCIEmail",
		"primary_mail_host":               "mail.example.com",
		"ociemail_region":                 "us-ashburn-1",
		"ociemail_timeout":                "1s",
		"ociemail_fail_hard":              true,
		"ociemail_username":               "ocid1.user",
		"ociemail_password":               "secret",
		"ociemail_source_tmpl":            "${hdr_from}",
		"ociemail_to_tmpl":                "${rcpt_to}",
		"ociemail_include_delivery_header": false,
		"ociemail_max_bytes":              2097152,
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Shutdown() }()

	_ = g.Process(e)

	if fake.lastReq == nil {
		t.Fatalf("expected OCI Email to be called")
	}
	if fake.lastReq.Host != "smtp.email.us-ashburn-1.oci.oraclecloud.com" {
		t.Fatalf("unexpected host: %q", fake.lastReq.Host)
	}
	if fake.lastReq.From != "support@domain.example" {
		t.Fatalf("unexpected From: %q", fake.lastReq.From)
	}
	if len(fake.lastReq.To) != 1 || fake.lastReq.To[0] != "bob@rcpt.example" {
		t.Fatalf("unexpected To: %#v", fake.lastReq.To)
	}
	if len(fake.lastReq.Raw) == 0 {
		t.Fatalf("expected raw message to be set")
	}
}

func TestOCIEmailProcessorFailHard(t *testing.T) {
	origNewSender := newOCIEmailSender
	defer func() { newOCIEmailSender = origNewSender }()

	fake := &fakeOCIEmailSender{err: errors.New("smtp send failed")}
	newOCIEmailSender = func(_ *ociemailConfig) ociemailSenderAPI {
		return fake
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.MailFrom = mail.Address{User: "alice", Host: "from.example"}
	e.RcptTo = append(e.RcptTo, mail.Address{User: "bob", Host: "rcpt.example"})
	_, _ = e.Data.WriteString("From: support@domain.example\r\nSubject: Hello\r\n\r\nBody")

	l, _ := log.GetLogger("./test_ociemail_fail.log", "debug")
	g, err := New(BackendConfig{
		"save_process":         "HeadersParser|Header|OCIEmail",
		"primary_mail_host":    "mail.example.com",
		"ociemail_host":        "smtp.email.us-ashburn-1.oci.oraclecloud.com",
		"ociemail_username":    "ocid1.user",
		"ociemail_password":    "secret",
		"ociemail_source_tmpl": "${hdr_from}",
		"ociemail_to_tmpl":     "${rcpt_to}",
		"ociemail_fail_hard":   true,
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Shutdown() }()

	res := g.Process(e)
	if res.Code() < 400 {
		t.Fatalf("expected backend failure, got: %v", res)
	}
}

func TestIsOCIAuthFailure(t *testing.T) {
	if !isOCIAuthFailure(errors.New("535 Authentication required")) {
		t.Fatal("expected 535 to be auth failure")
	}
	if isOCIAuthFailure(errors.New("connection reset")) {
		t.Fatal("expected non-535 error to not be auth failure")
	}
}

func TestOCIEmailTemplate_HeaderFrom(t *testing.T) {
	e := mail.NewEnvelope("127.0.0.1", 1)
	e.RcptTo = append(e.RcptTo, mail.Address{User: "bob", Host: "rcpt.example"})
	_, _ = e.Data.WriteString("From: support@domain.example\r\nSubject: Hi\r\n\r\nBody")

	vars := makeSESVars(e)
	if vars["hdr_from"] != "support@domain.example" {
		t.Fatalf("expected hdr_from, got: %q", vars["hdr_from"])
	}
	got := applySESTemplate("${hdr_from}", vars)
	if got != "support@domain.example" {
		t.Fatalf("unexpected template result: %q", got)
	}
}

func TestRewriteFrom_OCIEmailSourceTmpl(t *testing.T) {
	defer Svc.reset()

	deco := RewriteFrom()
	p := deco(ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
		return NewResult("250 OK"), nil
	}))

	if err := Svc.initialize(BackendConfig{
		"ociemail_source_tmpl": "sender@example.com",
	}); len(err) > 0 {
		t.Fatalf("init errors: %v", err)
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	_, _ = e.Data.WriteString("From: Old <old@example.com>\nTo: a@example.com\n\nBody")

	_, err := p.Process(e, TaskSaveMail)
	if err != nil {
		t.Fatal(err)
	}
	got := e.Data.String()
	if !strings.HasPrefix(got, "From: sender@example.com\n") {
		t.Fatalf("expected rewritten From, got: %q", got)
	}
}
