package backends

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/response"
)

// ----------------------------------------------------------------------------------
// Processor Name: ses
// ----------------------------------------------------------------------------------
// Description   : Sends the received RFC5322 email via AWS SES (SendRawEmail)
// ----------------------------------------------------------------------------------
// Config Options:
//
//	ses_region string - AWS region, eg "us-east-1"
//	ses_timeout string (omitempty) - eg "10s"
//	ses_fail_hard bool (omitempty) - if true, fail SMTP transaction on SES failure
//	ses_source_tmpl string - template for Source address
//	ses_to_tmpl string - template for Destinations (comma/semicolon/space separated)
//	ses_include_delivery_header bool (omitempty) - include e.DeliveryHeader in raw message
//	ses_max_bytes int (omitempty) - max raw message bytes (default 10485760)
//	ses_access_key_id string (omitempty) - optional explicit credentials
//	ses_secret_access_key string (omitempty) - optional explicit credentials
//
// ----------------------------------------------------------------------------------
func init() {
	processors["ses"] = func() Decorator {
		return SES()
	}
}

type sesConfig struct {
	Region                string `json:"ses_region"`
	Timeout               string `json:"ses_timeout,omitempty"`
	FailHard              bool   `json:"ses_fail_hard,omitempty"`
	SourceTmpl            string `json:"ses_source_tmpl"`
	ToTmpl                string `json:"ses_to_tmpl"`
	IncludeDeliveryHeader bool   `json:"ses_include_delivery_header,omitempty"`
	MaxBytes              int    `json:"ses_max_bytes,omitempty"`
	AccessKeyID           string `json:"ses_access_key_id,omitempty"`
	SecretAccessKey       string `json:"ses_secret_access_key,omitempty"`
}

type sesAPI interface {
	SendRawEmailWithContext(aws.Context, *ses.SendRawEmailInput, ...request.Option) (*ses.SendRawEmailOutput, error)
}

type sesSender struct {
	client sesAPI
	cfg    *sesConfig
}

var sesVarRe = regexp.MustCompile(`\$\{([a-zA-Z0-9_]+)\}`)

var newSESClient = func(sess *session.Session) sesAPI {
	return ses.New(sess)
}

func SES() Decorator {
	var sender *sesSender

	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		region, _ := backendConfig["ses_region"].(string)
		if strings.TrimSpace(region) == "" {
			// SES not configured; skip when using another outbound processor (eg OCIEmail).
			return nil
		}

		configType := BaseConfig(&sesConfig{})
		bcfg, err := Svc.ExtractConfig(backendConfig, configType)
		if err != nil {
			return err
		}

		cfg := bcfg.(*sesConfig)
		if strings.TrimSpace(cfg.SourceTmpl) == "" {
			return fmt.Errorf("ses_source_tmpl is required")
		}
		if strings.TrimSpace(cfg.ToTmpl) == "" {
			return fmt.Errorf("ses_to_tmpl is required")
		}

		timeout := time.Second * 10
		if strings.TrimSpace(cfg.Timeout) != "" {
			d, derr := time.ParseDuration(strings.TrimSpace(cfg.Timeout))
			if derr != nil {
				return fmt.Errorf("invalid ses_timeout: %w", derr)
			}
			timeout = d
		}

		if cfg.MaxBytes <= 0 {
			// SES raw email maximum is 10MB; keep this as a safe default.
			cfg.MaxBytes = 10 * 1024 * 1024
		}

		awsCfg := aws.NewConfig().
			WithRegion(cfg.Region).
			WithHTTPClient(&http.Client{Timeout: timeout})

		if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
			awsCfg = awsCfg.WithCredentials(credentials.NewStaticCredentials(
				strings.TrimSpace(cfg.AccessKeyID),
				strings.TrimSpace(cfg.SecretAccessKey),
				"",
			))
		}

		sess, serr := session.NewSession(awsCfg)
		if serr != nil {
			return serr
		}

		sender = &sesSender{
			client: newSESClient(sess),
			cfg:    cfg,
		}
		return nil
	}))

	return func(p Processor) Processor {
		return ProcessWith(func(e *mail.Envelope, task SelectTask) (Result, error) {
			if task != TaskSaveMail {
				return p.Process(e, task)
			}
			if sender == nil || sender.client == nil || sender.cfg == nil {
				err := fmt.Errorf("ses processor not initialized")
				if sender != nil && sender.cfg != nil && !sender.cfg.FailHard {
					Log().WithError(err).Warn("SES not initialized; skipping send")
					return p.Process(e, task)
				}
				return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
			}

			vars := makeSESVars(e)
			source := strings.TrimSpace(applySESTemplate(sender.cfg.SourceTmpl, vars))
			source = normalizeEmailAddress(source)
			dests := splitAddresses(applySESTemplate(sender.cfg.ToTmpl, vars))

			if source == "" || len(dests) == 0 {
				err := fmt.Errorf("ses template produced empty source or destinations")
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
				}
				Log().WithError(err).Warn("SES template invalid; skipping send")
				return p.Process(e, task)
			}

			raw, rerr := buildRawMessage(e, sender.cfg.IncludeDeliveryHeader)
			if rerr != nil {
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, rerr), rerr
				}
				Log().WithError(rerr).Warn("SES raw message build failed; skipping send")
				return p.Process(e, task)
			}
			if len(raw) > sender.cfg.MaxBytes {
				err := fmt.Errorf("ses raw message too large: %d bytes > %d", len(raw), sender.cfg.MaxBytes)
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
				}
				Log().WithError(err).Warn("SES message too large; skipping send")
				return p.Process(e, task)
			}

			input := &ses.SendRawEmailInput{
				RawMessage:   &ses.RawMessage{Data: raw},
				Source:       aws.String(source),
				Destinations: aws.StringSlice(dests),
			}

			ctx := context.Background()
			// Note: HTTPClient timeout already enforces an upper bound; context here is a best-effort guard.
			if strings.TrimSpace(sender.cfg.Timeout) != "" {
				if d, err := time.ParseDuration(strings.TrimSpace(sender.cfg.Timeout)); err == nil && d > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, d)
					defer cancel()
				}
			}

			_, err := sender.client.SendRawEmailWithContext(ctx, input)
			if err != nil {
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
				}
				Log().WithError(err).Warn("SES send failed; continuing")
			}

			return p.Process(e, task)
		})
	}
}

func buildRawMessage(e *mail.Envelope, includeDeliveryHeader bool) ([]byte, error) {
	if includeDeliveryHeader {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(e.NewReader()); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return append([]byte(nil), e.Data.Bytes()...), nil
}

func makeSESVars(e *mail.Envelope) map[string]string {
	vars := map[string]string{
		"remote_ip": e.RemoteIP,
		"helo":      e.Helo,
		"tls_on":    fmt.Sprintf("%t", e.TLS),
		"subject":   e.Subject,
	}

	mf := e.MailFrom.String()
	vars["mail_from"] = mf
	vars["mail_from_user"] = e.MailFrom.User
	vars["mail_from_host"] = e.MailFrom.Host

	if len(e.RcptTo) > 0 {
		vars["rcpt_to"] = e.RcptTo[0].String()
		vars["rcpt_user"] = e.RcptTo[0].User
		vars["rcpt_host"] = e.RcptTo[0].Host
	}
	if len(e.RcptTo) > 1 {
		all := make([]string, 0, len(e.RcptTo))
		for i := range e.RcptTo {
			all = append(all, e.RcptTo[i].String())
		}
		vars["rcpt_to_all"] = strings.Join(all, ",")
	} else {
		vars["rcpt_to_all"] = vars["rcpt_to"]
	}

	// Header-derived variables.
	// Requires headers to be parsed (eg pipeline contains HeadersParser|Header).
	// Best-effort: if not parsed, try parsing from e.Data.
	if e.Header == nil {
		_ = e.ParseHeaders()
	}
	if e.Header != nil {
		for k, vv := range e.Header {
			key := headerVarKey(k)
			if key == "" {
				continue
			}
			if len(vv) == 0 {
				continue
			}
			// If there are multiple values, join them. This is uncommon but valid.
			vars[key] = strings.TrimSpace(strings.Join(vv, ","))
		}
	}

	// Processor-injected values (survive header reparse in RewriteFrom).
	if e.Values != nil {
		if v, ok := e.Values["alias_reply_as"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				vars["alias_reply_as"] = v
				if strings.TrimSpace(vars["hdr_x_guerrilla_reply_as"]) == "" {
					vars["hdr_x_guerrilla_reply_as"] = v
				}
			}
		}
	}

	return vars
}

func headerVarKey(headerName string) string {
	// Normalize header name into a template-safe variable key.
	// Example: "X-Original-To" -> "hdr_x_original_to"
	s := strings.ToLower(strings.TrimSpace(headerName))
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, s)
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	if s == "" {
		return ""
	}
	return "hdr_" + s
}

func applySESTemplate(tmpl string, vars map[string]string) string {
	return sesVarRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := sesVarRe.FindStringSubmatch(m)
		if len(sub) != 2 {
			return m
		}
		if v, ok := vars[sub[1]]; ok {
			return v
		}
		return ""
	})
}

func splitAddresses(s string) []string {
	// split on comma/semicolon/whitespace
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(f))
	for _, item := range f {
		a := strings.TrimSpace(item)
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}
