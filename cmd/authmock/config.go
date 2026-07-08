package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type mockConfig struct {
	Listen          string            `json:"listen,omitempty"`
	RequiredHeaders map[string]string `json:"required_headers,omitempty"`
	Users           []mockUser        `json:"users"`
	Tenants         []mockTenant      `json:"tenants"`
}

type mockUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TenantID string `json:"tenant_id"`
}

type mockTenant struct {
	TenantID     string          `json:"tenant_id"`
	Provider     string          `json:"provider"`
	SES          *mockSES        `json:"ses,omitempty"`
	OCIEmail     *mockOCIEmail   `json:"ociemail,omitempty"`
	POP3Accounts []mockPOP3      `json:"pop3_accounts,omitempty"`
}

type mockSES struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

type mockOCIEmail struct {
	Host     string `json:"host,omitempty"`
	Region   string `json:"region,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type mockPOP3 struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
	User     string `json:"user"`
	Password string `json:"password"`
}

type authResponse struct {
	OK       bool         `json:"ok"`
	Error    string       `json:"error,omitempty"`
	TenantID string       `json:"tenant_id,omitempty"`
	Provider string       `json:"provider,omitempty"`
	SES      *mockSES     `json:"ses,omitempty"`
	OCIEmail *mockOCIEmail `json:"ociemail,omitempty"`
}

type tenantsResponse struct {
	Tenants []mockTenant `json:"tenants"`
}

func loadMockConfig(path string) (*mockConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg mockConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		cfg.Listen = "127.0.0.1:8080"
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *mockConfig) validate() error {
	if len(c.Tenants) == 0 {
		return fmt.Errorf("tenants must not be empty")
	}
	seen := make(map[string]struct{}, len(c.Tenants))
	for i, t := range c.Tenants {
		id := strings.TrimSpace(t.TenantID)
		if id == "" {
			return fmt.Errorf("tenants[%d].tenant_id is required", i)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate tenant_id %q", id)
		}
		seen[id] = struct{}{}
		provider := strings.ToLower(strings.TrimSpace(t.Provider))
		switch provider {
		case "ses":
			if t.SES == nil || strings.TrimSpace(t.SES.Region) == "" {
				return fmt.Errorf("tenants[%d] (%s): ses.region is required", i, id)
			}
		case "oci":
			if t.OCIEmail == nil || strings.TrimSpace(t.OCIEmail.Username) == "" {
				return fmt.Errorf("tenants[%d] (%s): ociemail.username is required", i, id)
			}
		default:
			return fmt.Errorf("tenants[%d] (%s): provider must be ses or oci", i, id)
		}
	}
	for i, u := range c.Users {
		if strings.TrimSpace(u.Username) == "" {
			return fmt.Errorf("users[%d].username is required", i)
		}
		if u.Password == "" {
			return fmt.Errorf("users[%d].password is required", i)
		}
		if strings.TrimSpace(u.TenantID) == "" {
			return fmt.Errorf("users[%d].tenant_id is required", i)
		}
		if _, ok := seen[strings.TrimSpace(u.TenantID)]; !ok {
			return fmt.Errorf("users[%d] references unknown tenant_id %q", i, u.TenantID)
		}
	}
	return nil
}

func (c *mockConfig) findUser(username, password string) (*mockUser, bool) {
	username = strings.TrimSpace(username)
	for i := range c.Users {
		u := &c.Users[i]
		if u.Username == username && u.Password == password {
			return u, true
		}
	}
	return nil, false
}

func (c *mockConfig) findTenant(tenantID string) (*mockTenant, bool) {
	tenantID = strings.TrimSpace(tenantID)
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if t.TenantID == tenantID {
			return t, true
		}
	}
	return nil, false
}

func (t *mockTenant) authResponse() authResponse {
	return authResponse{
		OK:       true,
		TenantID: t.TenantID,
		Provider: t.Provider,
		SES:      t.SES,
		OCIEmail: t.OCIEmail,
	}
}

func (c *mockConfig) checkRequiredHeaders(r *http.Request) error {
	for name, want := range c.RequiredHeaders {
		got := strings.TrimSpace(r.Header.Get(name))
		if got != want {
			return fmt.Errorf("missing or invalid header %s", name)
		}
	}
	return nil
}
