package backends

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/flashmob/go-guerrilla/log"
	"github.com/flashmob/go-guerrilla/mail"
)

type fakeSES struct {
	lastInput *ses.SendRawEmailInput
	err       error
}

func (f *fakeSES) SendRawEmailWithContext(_ aws.Context, input *ses.SendRawEmailInput, _ ...request.Option) (*ses.SendRawEmailOutput, error) {
	f.lastInput = input
	if f.err != nil {
		return nil, f.err
	}
	return &ses.SendRawEmailOutput{MessageId: aws.String("fake")}, nil
}

func TestSESProcessorSendRawEmailTemplates(t *testing.T) {
	origNewClient := newSESClient
	defer func() { newSESClient = origNewClient }()

	fake := &fakeSES{}
	newSESClient = func(_ *session.Session) sesAPI {
		return fake
	}

	e := mail.NewEnvelope("127.0.0.1", 1)
	e.MailFrom = mail.Address{User: "alice", Host: "from.example"}
	e.RcptTo = append(e.RcptTo, mail.Address{User: "bob", Host: "rcpt.example"})
	_, _ = e.Data.WriteString("Subject: Hello\r\nX-Original-To: user2@example.com\r\n\r\nBody")

	l, _ := log.GetLogger("./test_ses.log", "debug")
	g, err := New(BackendConfig{
		"save_process":                "HeadersParser|Header|SES",
		"primary_mail_host":           "mail.example.com",
		"ses_region":                  "us-east-1",
		"ses_timeout":                 "1s",
		"ses_fail_hard":               true,
		"ses_source_tmpl":             "sender+${rcpt_user}@example.com",
		"ses_to_tmpl":                 "to+${mail_from_user}@example.com",
		"ses_include_delivery_header": false,
		"ses_max_bytes":               10485760,
	}, l)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Shutdown() }()

	_ = g.Process(e)

	if fake.lastInput == nil {
		t.Fatalf("expected SES to be called")
	}
	if fake.lastInput.Source == nil || *fake.lastInput.Source != "sender+bob@example.com" {
		t.Fatalf("unexpected Source: %#v", fake.lastInput.Source)
	}
	if len(fake.lastInput.Destinations) != 1 || *fake.lastInput.Destinations[0] != "to+alice@example.com" {
		t.Fatalf("unexpected Destinations: %#v", fake.lastInput.Destinations)
	}
	if fake.lastInput.RawMessage == nil || len(fake.lastInput.RawMessage.Data) == 0 {
		t.Fatalf("expected RawMessage.Data to be set")
	}
}

func TestSESTemplate_HeaderVars(t *testing.T) {
	e := mail.NewEnvelope("127.0.0.1", 1)
	_, _ = e.Data.WriteString("Subject: Hi\r\nX-Original-To: user2@example.com\r\n\r\nBody")
	// ensure header vars are populated even if the pipeline didn't parse headers yet
	vars := makeSESVars(e)
	if vars["hdr_x_original_to"] != "user2@example.com" {
		t.Fatalf("expected hdr_x_original_to to be set, got: %q", vars["hdr_x_original_to"])
	}
	got := applySESTemplate("${hdr_x_original_to}", vars)
	if got != "user2@example.com" {
		t.Fatalf("expected template to resolve header var, got: %q", got)
	}
}
