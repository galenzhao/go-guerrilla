package backends

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseTenantsList(t *testing.T) {
	body := []byte(`{
		"tenants": [
			{
				"tenant_id": "acme",
				"provider": "oci",
				"ociemail": {"region":"us-phoenix-1","username":"u","password":"p"},
				"pop3_accounts": [{"host":"pop.example.com","user":"a@acme.com","password":"pw"}]
			}
		]
	}`)
	tenants, err := ParseTenantsList(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(tenants) != 1 || tenants[0].TenantID != "acme" {
		t.Fatalf("unexpected tenants: %+v", tenants)
	}
	if len(tenants[0].POP3Accounts) != 1 {
		t.Fatalf("expected 1 pop3 account, got %+v", tenants[0].POP3Accounts)
	}
}

func TestHTTPTenantRegistryRefresh(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tenants": [{
				"tenant_id":"beta",
				"provider":"ses",
				"ses":{"region":"us-east-1","access_key_id":"k","secret_access_key":"s"},
				"pop3_accounts":[{"host":"pop.test.com","user":"b@test.com","password":"pw"}]
			}]
		}`))
	}))
	defer s.Close()

	reg, err := NewHTTPTenantRegistry(TenantRegistryConfig{
		URL:     s.URL,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	tenant, ok := reg.Get("beta")
	if !ok || tenant.Provider != "ses" {
		t.Fatalf("tenant lookup failed: %+v", tenant)
	}
	pop3 := reg.POP3Accounts()
	if len(pop3) != 1 || pop3[0].TenantID != "beta" {
		t.Fatalf("unexpected pop3 accounts: %+v", pop3)
	}
}

func TestHTTPTenantRegistryRefreshKeepsCacheOnFailure(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"tenants":[{"tenant_id":"acme","provider":"oci","ociemail":{"region":"r","username":"u","password":"p"}}]}`))
			return
		}
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer s.Close()

	reg, err := NewHTTPTenantRegistry(TenantRegistryConfig{URL: s.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
	if _, ok := reg.Get("acme"); !ok {
		t.Fatal("expected cached tenant after failed refresh")
	}
}

func TestValidateTenantSendConfig(t *testing.T) {
	_, err := validateTenantSendConfig(&TenantSendConfig{TenantID: "x", Provider: "oci"})
	if err == nil {
		t.Fatal("expected missing ociemail error")
	}
	cfg, err := validateTenantSendConfig(&TenantSendConfig{
		TenantID: "x",
		Provider: "ses",
		SES:      &TenantSES{Region: "us-east-1"},
	})
	if err != nil || cfg.Provider != "ses" {
		t.Fatalf("unexpected: cfg=%+v err=%v", cfg, err)
	}
}
