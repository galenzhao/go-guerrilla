package backends

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/response"
)

// ----------------------------------------------------------------------------------
// Processor Name: ociemail
// ----------------------------------------------------------------------------------
// Description   : Sends the received RFC5322 email via Oracle Cloud Email Delivery SMTP
// ----------------------------------------------------------------------------------
// Config Options:
//
//	ociemail_host string (omitempty) - SMTP host; if empty, built from ociemail_region
//	ociemail_region string (omitempty) - OCI region, eg "us-ashburn-1"
//	ociemail_port int (omitempty) - default 587 (STARTTLS); 465 uses implicit TLS
//	ociemail_username string - SMTP username (required)
//	ociemail_password string - SMTP password (required)
//	ociemail_timeout string (omitempty) - eg "10s"
//	ociemail_fail_hard bool (omitempty) - if true, fail SMTP transaction on send failure
//	ociemail_source_tmpl string - template for MAIL FROM address
//	ociemail_to_tmpl string - template for RCPT TO addresses (comma/semicolon/space separated)
//	ociemail_include_delivery_header bool (omitempty) - include e.DeliveryHeader in raw message
//	ociemail_max_bytes int (omitempty) - max raw message bytes (default 2097152)
//
// ----------------------------------------------------------------------------------
func init() {
	processors["ociemail"] = func() Decorator {
		return OCIEmail()
	}
}

const (
	defaultOCIEmailPort     = 587
	defaultOCIEmailMaxBytes = 2 * 1024 * 1024
	ociEmailHostPrefix      = "smtp.email."
	ociEmailHostSuffix      = ".oci.oraclecloud.com"
)

type ociemailConfig struct {
	Host                  string `json:"ociemail_host,omitempty"`
	Region                string `json:"ociemail_region,omitempty"`
	Port                  int    `json:"ociemail_port,omitempty"`
	Username              string `json:"ociemail_username"`
	Password              string `json:"ociemail_password"`
	Timeout               string `json:"ociemail_timeout,omitempty"`
	FailHard              bool   `json:"ociemail_fail_hard,omitempty"`
	SourceTmpl            string `json:"ociemail_source_tmpl"`
	ToTmpl                string `json:"ociemail_to_tmpl"`
	IncludeDeliveryHeader bool   `json:"ociemail_include_delivery_header,omitempty"`
	MaxBytes              int    `json:"ociemail_max_bytes,omitempty"`
}

type ociemailSendRequest struct {
	Host string
	From string
	To   []string
	Raw  []byte
}

type ociemailSenderAPI interface {
	Send(ctx context.Context, cfg *ociemailConfig, req *ociemailSendRequest) error
}

type ociemailSender struct {
	client ociemailSenderAPI
	cfg    *ociemailConfig
}

var newOCIEmailSender = func(cfg *ociemailConfig) ociemailSenderAPI {
	return &smtpOCIEmailSender{cfg: cfg}
}

func OCIEmail() Decorator {
	var sender *ociemailSender

	Svc.AddInitializer(InitializeWith(func(backendConfig BackendConfig) error {
		username, _ := backendConfig["ociemail_username"].(string)
		region, _ := backendConfig["ociemail_region"].(string)
		host, _ := backendConfig["ociemail_host"].(string)
		if strings.TrimSpace(username) == "" && strings.TrimSpace(region) == "" && strings.TrimSpace(host) == "" {
			// OCI Email not configured; skip when using another outbound processor (eg SES).
			return nil
		}

		configType := BaseConfig(&ociemailConfig{})
		bcfg, err := Svc.ExtractConfig(backendConfig, configType)
		if err != nil {
			return err
		}

		cfg := bcfg.(*ociemailConfig)
		if _, err := resolveOCIEmailHost(cfg); err != nil {
			return err
		}
		if strings.TrimSpace(cfg.Username) == "" {
			return fmt.Errorf("ociemail_username is required")
		}
		if strings.TrimSpace(cfg.Password) == "" {
			return fmt.Errorf("ociemail_password is required")
		}
		if strings.TrimSpace(cfg.SourceTmpl) == "" {
			return fmt.Errorf("ociemail_source_tmpl is required")
		}
		if strings.TrimSpace(cfg.ToTmpl) == "" {
			return fmt.Errorf("ociemail_to_tmpl is required")
		}

		if strings.TrimSpace(cfg.Timeout) != "" {
			if _, derr := time.ParseDuration(strings.TrimSpace(cfg.Timeout)); derr != nil {
				return fmt.Errorf("invalid ociemail_timeout: %w", derr)
			}
		}

		if cfg.Port <= 0 {
			cfg.Port = defaultOCIEmailPort
		}
		if cfg.MaxBytes <= 0 {
			cfg.MaxBytes = defaultOCIEmailMaxBytes
		}

		sender = &ociemailSender{
			client: newOCIEmailSender(cfg),
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
				err := fmt.Errorf("ociemail processor not initialized")
				if sender != nil && sender.cfg != nil && !sender.cfg.FailHard {
					Log().WithError(err).Warn("OCI Email not initialized; skipping send")
					return p.Process(e, task)
				}
				return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
			}

			vars := makeSESVars(e)
			source := strings.TrimSpace(applySESTemplate(sender.cfg.SourceTmpl, vars))
			source = normalizeEmailAddress(source)
			dests := splitAddresses(applySESTemplate(sender.cfg.ToTmpl, vars))

			if source == "" || len(dests) == 0 {
				err := fmt.Errorf("ociemail template produced empty source or destinations")
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
				}
				Log().WithError(err).Warn("OCI Email template invalid; skipping send")
				return p.Process(e, task)
			}

			raw, rerr := buildRawMessage(e, sender.cfg.IncludeDeliveryHeader)
			if rerr != nil {
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, rerr), rerr
				}
				Log().WithError(rerr).Warn("OCI Email raw message build failed; skipping send")
				return p.Process(e, task)
			}
			if len(raw) > sender.cfg.MaxBytes {
				err := fmt.Errorf("ociemail raw message too large: %d bytes > %d", len(raw), sender.cfg.MaxBytes)
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
				}
				Log().WithError(err).Warn("OCI Email message too large; skipping send")
				return p.Process(e, task)
			}

			host, herr := resolveOCIEmailHost(sender.cfg)
			if herr != nil {
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, herr), herr
				}
				Log().WithError(herr).Warn("OCI Email host resolution failed; skipping send")
				return p.Process(e, task)
			}

			ctx := context.Background()
			if d := ociemailTimeout(sender.cfg); d > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, d)
				defer cancel()
			}

			err := sender.client.Send(ctx, sender.cfg, &ociemailSendRequest{
				Host: host,
				From: source,
				To:   dests,
				Raw:  raw,
			})
			if err != nil {
				if sender.cfg.FailHard {
					return NewResult(response.Canned.FailBackendTransaction, response.SP, err), err
				}
				Log().WithError(err).Warn("OCI Email send failed; continuing")
			}

			return p.Process(e, task)
		})
	}
}

func resolveOCIEmailHost(cfg *ociemailConfig) (string, error) {
	if h := strings.TrimSpace(cfg.Host); h != "" {
		return h, nil
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		return "", fmt.Errorf("ociemail_host or ociemail_region is required")
	}
	return ociEmailHostPrefix + region + ociEmailHostSuffix, nil
}

func ociemailTimeout(cfg *ociemailConfig) time.Duration {
	if cfg == nil || strings.TrimSpace(cfg.Timeout) == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(strings.TrimSpace(cfg.Timeout))
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

type smtpOCIEmailSender struct {
	cfg *ociemailConfig
}

func (s *smtpOCIEmailSender) Send(ctx context.Context, cfg *ociemailConfig, req *ociemailSendRequest) error {
	timeout := ociemailTimeout(cfg)
	var lastErr error
	backoff := 200 * time.Millisecond

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		err := sendOCIEmailSMTP(ctx, cfg, req, timeout)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isOCIAuthFailure(err) || attempt == 2 {
			return err
		}
	}

	return lastErr
}

func sendOCIEmailSMTP(ctx context.Context, cfg *ociemailConfig, req *ociemailSendRequest, timeout time.Duration) error {
	addr := net.JoinHostPort(req.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: timeout}

	var client *smtp.Client
	if cfg.Port == 465 {
		tlsCfg := &tls.Config{
			ServerName: req.Host,
			MinVersion: tls.VersionTLS12,
		}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err != nil {
			return err
		}
		client, err = smtp.NewClient(conn, req.Host)
		if err != nil {
			_ = conn.Close()
			return err
		}
	} else {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		client, err = smtp.NewClient(conn, req.Host)
		if err != nil {
			_ = conn.Close()
			return err
		}
		if err := client.StartTLS(&tls.Config{
			ServerName: req.Host,
			MinVersion: tls.VersionTLS12,
		}); err != nil {
			_ = client.Close()
			return err
		}
	}
	defer func() { _ = client.Close() }()

	auth := smtp.PlainAuth("", strings.TrimSpace(cfg.Username), cfg.Password, req.Host)
	if err := client.Auth(auth); err != nil {
		return err
	}
	if err := client.Mail(req.From); err != nil {
		return err
	}
	for _, to := range req.To {
		if err := client.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(req.Raw); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func isOCIAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "535")
}
