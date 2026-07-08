package backends

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/flashmob/go-guerrilla/mail"
)

const envelopeTenantSendKey = "tenant_send"

// TenantSendConfig holds outbound send credentials for a tenant.
type TenantSendConfig struct {
	TenantID string       `json:"tenant_id"`
	Provider string       `json:"provider"`
	SES      *TenantSES   `json:"ses,omitempty"`
	OCIEmail *TenantOCI   `json:"ociemail,omitempty"`
}

// TenantSES holds AWS SES credentials for a tenant.
type TenantSES struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// TenantOCI holds OCI Email Delivery SMTP credentials for a tenant.
type TenantOCI struct {
	Host     string `json:"host,omitempty"`
	Region   string `json:"region,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// TenantPOP3 is a POP3 mailbox used for alias indexing.
type TenantPOP3 struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// Tenant is a full tenant record from the registry.
type Tenant struct {
	TenantID     string         `json:"tenant_id"`
	Provider     string         `json:"provider"`
	SES          *TenantSES     `json:"ses,omitempty"`
	OCIEmail     *TenantOCI     `json:"ociemail,omitempty"`
	POP3Accounts []TenantPOP3   `json:"pop3_accounts,omitempty"`
}

// TaggedPOP3Account is a POP3 account with its owning tenant.
type TaggedPOP3Account struct {
	TenantID string
	Account  AliasPOP3Account
}

// tenantsListResponse is the JSON body from GET /tenants.
type tenantsListResponse struct {
	Tenants []tenantJSON `json:"tenants"`
}

// tenantJSON is used for parsing both auth and registry responses.
type tenantJSON struct {
	TenantID     string          `json:"tenant_id"`
	Provider     string          `json:"provider"`
	SES          *TenantSES      `json:"ses,omitempty"`
	OCIEmail     *TenantOCI      `json:"ociemail,omitempty"`
	POP3Accounts []TenantPOP3    `json:"pop3_accounts,omitempty"`
	OK           bool            `json:"ok,omitempty"`
}

// ParseTenantSendConfigFromAuth parses tenant send config from an auth response body.
// Returns nil when ok is true but no tenant fields are present (backward compatible).
func ParseTenantSendConfigFromAuth(body []byte) (*TenantSendConfig, error) {
	var raw tenantJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw.TenantID) == "" {
		return nil, nil
	}
	return validateTenantSendConfig(&TenantSendConfig{
		TenantID: raw.TenantID,
		Provider: raw.Provider,
		SES:      raw.SES,
		OCIEmail: raw.OCIEmail,
	})
}

// ParseTenantsList parses the GET /tenants response body.
func ParseTenantsList(body []byte) ([]Tenant, error) {
	var resp tenantsListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make([]Tenant, 0, len(resp.Tenants))
	for i, t := range resp.Tenants {
		tenant, err := parseTenantRecord(t)
		if err != nil {
			return nil, fmt.Errorf("tenants[%d]: %w", i, err)
		}
		out = append(out, *tenant)
	}
	return out, nil
}

func parseTenantRecord(raw tenantJSON) (*Tenant, error) {
	send, err := validateTenantSendConfig(&TenantSendConfig{
		TenantID: raw.TenantID,
		Provider: raw.Provider,
		SES:      raw.SES,
		OCIEmail: raw.OCIEmail,
	})
	if err != nil {
		return nil, err
	}
	t := &Tenant{
		TenantID:     send.TenantID,
		Provider:     send.Provider,
		SES:          send.SES,
		OCIEmail:     send.OCIEmail,
		POP3Accounts: raw.POP3Accounts,
	}
	for i := range t.POP3Accounts {
		if t.POP3Accounts[i].Port <= 0 {
			t.POP3Accounts[i].Port = 995
		}
		if !t.POP3Accounts[i].TLS {
			// default TLS true for POP3
			t.POP3Accounts[i].TLS = true
		}
	}
	return t, nil
}

func validateTenantSendConfig(cfg *TenantSendConfig) (*TenantSendConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("tenant config is nil")
	}
	cfg.TenantID = strings.TrimSpace(cfg.TenantID)
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.TenantID == "" {
		return nil, fmt.Errorf("tenant_id is required")
	}
	switch cfg.Provider {
	case "ses":
		if cfg.SES == nil {
			return nil, fmt.Errorf("ses block is required for provider ses")
		}
		cfg.SES.Region = strings.TrimSpace(cfg.SES.Region)
		if cfg.SES.Region == "" {
			return nil, fmt.Errorf("ses.region is required")
		}
	case "oci":
		if cfg.OCIEmail == nil {
			return nil, fmt.Errorf("ociemail block is required for provider oci")
		}
		cfg.OCIEmail.Username = strings.TrimSpace(cfg.OCIEmail.Username)
		cfg.OCIEmail.Password = cfg.OCIEmail.Password
		if cfg.OCIEmail.Username == "" {
			return nil, fmt.Errorf("ociemail.username is required")
		}
		if cfg.OCIEmail.Password == "" {
			return nil, fmt.Errorf("ociemail.password is required")
		}
		if cfg.OCIEmail.Port <= 0 {
			cfg.OCIEmail.Port = defaultOCIEmailPort
		}
	default:
		return nil, fmt.Errorf("provider must be ses or oci")
	}
	return cfg, nil
}

// TenantSendFromTenant builds a TenantSendConfig from a Tenant record.
func TenantSendFromTenant(t *Tenant) *TenantSendConfig {
	if t == nil {
		return nil
	}
	return &TenantSendConfig{
		TenantID: t.TenantID,
		Provider: t.Provider,
		SES:      t.SES,
		OCIEmail: t.OCIEmail,
	}
}

// SetEnvelopeTenantSend stores tenant send config on the envelope.
func SetEnvelopeTenantSend(e *mail.Envelope, cfg *TenantSendConfig) {
	if cfg == nil {
		return
	}
	if e.Values == nil {
		e.Values = make(map[string]interface{})
	}
	e.Values[envelopeTenantSendKey] = cfg
}
