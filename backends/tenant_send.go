package backends

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/flashmob/go-guerrilla/mail"
)

// GetEnvelopeTenantSend reads tenant send config from the envelope.
func GetEnvelopeTenantSend(e *mail.Envelope) *TenantSendConfig {
	if e == nil || e.Values == nil {
		return nil
	}
	cfg, ok := e.Values[envelopeTenantSendKey].(*TenantSendConfig)
	if !ok || cfg == nil {
		return nil
	}
	return cfg
}

type tenantSenderCache struct {
	mu    sync.RWMutex
	ses   map[string]sesAPI
	oci   map[string]ociemailSenderAPI
}

var globalTenantSenders = &tenantSenderCache{
	ses: make(map[string]sesAPI),
	oci: make(map[string]ociemailSenderAPI),
}

func tenantCacheKey(cfg *TenantSendConfig) string {
	if cfg == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s:%s:", cfg.TenantID, cfg.Provider)
	switch cfg.Provider {
	case "ses":
		if cfg.SES != nil {
			fmt.Fprintf(h, "%s:%s:%s", cfg.SES.Region, cfg.SES.AccessKeyID, cfg.SES.SecretAccessKey)
		}
	case "oci":
		if cfg.OCIEmail != nil {
			fmt.Fprintf(h, "%s:%s:%s:%s:%d", cfg.OCIEmail.Host, cfg.OCIEmail.Region, cfg.OCIEmail.Username, cfg.OCIEmail.Password, cfg.OCIEmail.Port)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func tenantSESClient(cfg *TenantSendConfig, timeout time.Duration) (sesAPI, error) {
	key := tenantCacheKey(cfg)
	globalTenantSenders.mu.RLock()
	if c, ok := globalTenantSenders.ses[key]; ok {
		globalTenantSenders.mu.RUnlock()
		return c, nil
	}
	globalTenantSenders.mu.RUnlock()

	if cfg.SES == nil || strings.TrimSpace(cfg.SES.Region) == "" {
		return nil, fmt.Errorf("tenant %s: ses config missing", cfg.TenantID)
	}
	awsCfg := aws.NewConfig().
		WithRegion(strings.TrimSpace(cfg.SES.Region)).
		WithHTTPClient(&http.Client{Timeout: timeout})
	if cfg.SES.AccessKeyID != "" || cfg.SES.SecretAccessKey != "" {
		awsCfg = awsCfg.WithCredentials(credentials.NewStaticCredentials(
			strings.TrimSpace(cfg.SES.AccessKeyID),
			strings.TrimSpace(cfg.SES.SecretAccessKey),
			"",
		))
	}
	sess, err := session.NewSession(awsCfg)
	if err != nil {
		return nil, err
	}
	client := newSESClient(sess)

	globalTenantSenders.mu.Lock()
	globalTenantSenders.ses[key] = client
	globalTenantSenders.mu.Unlock()
	return client, nil
}

func tenantOCIConfig(cfg *TenantSendConfig) (*ociemailConfig, error) {
	if cfg.OCIEmail == nil {
		return nil, fmt.Errorf("tenant %s: ociemail config missing", cfg.TenantID)
	}
	oci := cfg.OCIEmail
	port := oci.Port
	if port <= 0 {
		port = defaultOCIEmailPort
	}
	return &ociemailConfig{
		Host:     strings.TrimSpace(oci.Host),
		Region:   strings.TrimSpace(oci.Region),
		Port:     port,
		Username: strings.TrimSpace(oci.Username),
		Password: oci.Password,
	}, nil
}

func tenantOCIClient(cfg *TenantSendConfig) (ociemailSenderAPI, error) {
	key := tenantCacheKey(cfg)
	globalTenantSenders.mu.RLock()
	if c, ok := globalTenantSenders.oci[key]; ok {
		globalTenantSenders.mu.RUnlock()
		return c, nil
	}
	globalTenantSenders.mu.RUnlock()

	ociCfg, err := tenantOCIConfig(cfg)
	if err != nil {
		return nil, err
	}
	client := newOCIEmailSender(ociCfg)

	globalTenantSenders.mu.Lock()
	globalTenantSenders.oci[key] = client
	globalTenantSenders.mu.Unlock()
	return client, nil
}

// SendViaTenantSES sends raw email using tenant SES credentials.
func SendViaTenantSES(ctx context.Context, tenant *TenantSendConfig, globalCfg *sesConfig, source string, dests []string, raw []byte) error {
	if tenant == nil {
		return fmt.Errorf("tenant send config is nil")
	}
	if tenant.Provider != "ses" {
		return fmt.Errorf("tenant %s provider %q is not ses", tenant.TenantID, tenant.Provider)
	}
	timeout := time.Second * 10
	if globalCfg != nil && strings.TrimSpace(globalCfg.Timeout) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(globalCfg.Timeout)); err == nil && d > 0 {
			timeout = d
		}
	}
	client, err := tenantSESClient(tenant, timeout)
	if err != nil {
		return err
	}
	input := &ses.SendRawEmailInput{
		RawMessage:   &ses.RawMessage{Data: raw},
		Source:       aws.String(source),
		Destinations: aws.StringSlice(dests),
	}
	_, err = client.SendRawEmailWithContext(ctx, input)
	return err
}

// SendViaTenantOCI sends raw email using tenant OCI Email credentials.
func SendViaTenantOCI(ctx context.Context, tenant *TenantSendConfig, globalCfg *ociemailConfig, source string, dests []string, raw []byte) error {
	if tenant == nil {
		return fmt.Errorf("tenant send config is nil")
	}
	if tenant.Provider != "oci" {
		return fmt.Errorf("tenant %s provider %q is not oci", tenant.TenantID, tenant.Provider)
	}
	ociCfg, err := tenantOCIConfig(tenant)
	if err != nil {
		return err
	}
	host, err := resolveOCIEmailHost(ociCfg)
	if err != nil {
		return err
	}
	client, err := tenantOCIClient(tenant)
	if err != nil {
		return err
	}
	return client.Send(ctx, ociCfg, &ociemailSendRequest{
		Host: host,
		From: source,
		To:   dests,
		Raw:  raw,
	})
}
