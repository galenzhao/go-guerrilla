package guerrilla

import (
	"context"
	"strings"
	"time"

	"github.com/flashmob/go-guerrilla/backends"
)

// NewTenantRegistryFromConfig builds an HTTP tenant registry from app config.
func NewTenantRegistryFromConfig(c TenantRegistryConfig) (backends.TenantRegistry, error) {
	if strings.TrimSpace(c.URL) == "" {
		return nil, nil
	}
	timeout := 10 * time.Second
	if strings.TrimSpace(c.Timeout) != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return nil, err
		}
		timeout = d
	}
	pollInterval := 5 * time.Minute
	if strings.TrimSpace(c.PollInterval) != "" {
		d, err := time.ParseDuration(c.PollInterval)
		if err != nil {
			return nil, err
		}
		pollInterval = d
	}
	return backends.NewHTTPTenantRegistry(backends.TenantRegistryConfig{
		URL:          c.URL,
		PollInterval: pollInterval,
		Timeout:      timeout,
		Headers:      c.Headers,
	})
}

// StartTenantRegistryPoller refreshes the registry on startup and on interval until done closes.
func StartTenantRegistryPoller(registry backends.TenantRegistry, done <-chan struct{}) {
	if registry == nil {
		return
	}
	poll := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := registry.Refresh(ctx); err != nil {
			backends.Log().WithError(err).Warn("tenant registry refresh failed")
		}
	}
	poll()

	interval := 5 * time.Minute
	if hr, ok := registry.(interface{ PollInterval() time.Duration }); ok {
		if d := hr.PollInterval(); d > 0 {
			interval = d
		}
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
}
