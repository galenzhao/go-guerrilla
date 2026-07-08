package backends

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TenantRegistryConfig configures the HTTP tenant registry client.
type TenantRegistryConfig struct {
	URL          string
	PollInterval time.Duration
	Timeout      time.Duration
	Headers      map[string]string
}

// TenantRegistry provides cached tenant records from GET /tenants.
type TenantRegistry interface {
	Refresh(ctx context.Context) error
	All() []Tenant
	Get(tenantID string) (*Tenant, bool)
	POP3Accounts() []TaggedPOP3Account
	PollInterval() time.Duration
}

type httpTenantRegistry struct {
	cfg      TenantRegistryConfig
	client   *http.Client
	mu       sync.RWMutex
	tenants  map[string]Tenant
	pop3List []TaggedPOP3Account
}

var (
	globalTenantRegistry   TenantRegistry
	globalTenantRegistryMu sync.RWMutex
)

// SetGlobalTenantRegistry sets the process-wide tenant registry used by AliasResolve.
func SetGlobalTenantRegistry(r TenantRegistry) {
	globalTenantRegistryMu.Lock()
	globalTenantRegistry = r
	globalTenantRegistryMu.Unlock()
}

// GlobalTenantRegistry returns the process-wide tenant registry, if configured.
func GlobalTenantRegistry() TenantRegistry {
	globalTenantRegistryMu.RLock()
	defer globalTenantRegistryMu.RUnlock()
	return globalTenantRegistry
}

// NewHTTPTenantRegistry creates a registry client for GET /tenants.
func NewHTTPTenantRegistry(cfg TenantRegistryConfig) (TenantRegistry, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("tenant_registry.url is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Minute
	}
	return &httpTenantRegistry{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		tenants: make(map[string]Tenant),
	}, nil
}

// TenantRegistryConfigFromMap parses tenant_registry from app config JSON.
func TenantRegistryConfigFromMap(raw map[string]interface{}) (TenantRegistryConfig, error) {
	cfg := TenantRegistryConfig{
		PollInterval: 5 * time.Minute,
		Timeout:      10 * time.Second,
	}
	if raw == nil {
		return cfg, fmt.Errorf("tenant_registry is nil")
	}
	if v, ok := raw["url"].(string); ok {
		cfg.URL = strings.TrimSpace(v)
	}
	if rawInterval, ok := raw["poll_interval"].(string); ok && strings.TrimSpace(rawInterval) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(rawInterval))
		if err != nil {
			return cfg, fmt.Errorf("invalid tenant_registry.poll_interval: %w", err)
		}
		cfg.PollInterval = d
	}
	if rawTimeout, ok := raw["timeout"].(string); ok && strings.TrimSpace(rawTimeout) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(rawTimeout))
		if err != nil {
			return cfg, fmt.Errorf("invalid tenant_registry.timeout: %w", err)
		}
		cfg.Timeout = d
	}
	if headers, ok := raw["headers"].(map[string]interface{}); ok {
		cfg.Headers = make(map[string]string, len(headers))
		for k, v := range headers {
			if s, ok := v.(string); ok {
				cfg.Headers[k] = s
			}
		}
	}
	return cfg, nil
}

func (r *httpTenantRegistry) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range r.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("tenant registry returned status %d", resp.StatusCode)
	}
	tenants, err := ParseTenantsList(body)
	if err != nil {
		return err
	}
	r.applyTenants(tenants)
	return nil
}

func (r *httpTenantRegistry) applyTenants(tenants []Tenant) {
	byID := make(map[string]Tenant, len(tenants))
	pop3 := make([]TaggedPOP3Account, 0)
	for _, t := range tenants {
		byID[t.TenantID] = t
		for _, p := range t.POP3Accounts {
			acc := AliasPOP3Account{
				Host:     strings.TrimSpace(p.Host),
				Port:     p.Port,
				TLS:      p.TLS,
				User:     strings.TrimSpace(p.User),
				Password: p.Password,
				TenantID: t.TenantID,
			}
			if acc.Port <= 0 {
				acc.Port = 995
			}
			pop3 = append(pop3, TaggedPOP3Account{TenantID: t.TenantID, Account: acc})
		}
	}
	r.mu.Lock()
	r.tenants = byID
	r.pop3List = pop3
	r.mu.Unlock()
}

func (r *httpTenantRegistry) All() []Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		out = append(out, t)
	}
	return out
}

func (r *httpTenantRegistry) Get(tenantID string) (*Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tenants[strings.TrimSpace(tenantID)]
	if !ok {
		return nil, false
	}
	copy := t
	return &copy, true
}

func (r *httpTenantRegistry) POP3Accounts() []TaggedPOP3Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TaggedPOP3Account, len(r.pop3List))
	copy(out, r.pop3List)
	return out
}

// PollInterval returns the configured registry poll interval.
func (r *httpTenantRegistry) PollInterval() time.Duration {
	return r.cfg.PollInterval
}
