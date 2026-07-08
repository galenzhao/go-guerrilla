package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMockConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authmock.conf")
	data := []byte(`{
		"listen": "127.0.0.1:9090",
		"users": [{"username":"u","password":"p","tenant_id":"acme"}],
		"tenants": [{
			"tenant_id": "acme",
			"provider": "oci",
			"ociemail": {"region":"us-phoenix-1","username":"user","password":"pass"},
			"pop3_accounts": [{"host":"pop.example.com","user":"a@acme.com","password":"pw"}]
		}]
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadMockConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9090" {
		t.Fatalf("listen: %q", cfg.Listen)
	}
	user, ok := cfg.findUser("u", "p")
	if !ok || user.TenantID != "acme" {
		t.Fatalf("user lookup failed: %+v", user)
	}
	tenant, ok := cfg.findTenant("acme")
	if !ok || tenant.Provider != "oci" {
		t.Fatalf("tenant lookup failed: %+v", tenant)
	}
}

func TestLoadMockConfigRejectsUnknownTenant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authmock.conf")
	data := []byte(`{
		"users": [{"username":"u","password":"p","tenant_id":"missing"}],
		"tenants": [{"tenant_id":"acme","provider":"oci","ociemail":{"region":"r","username":"u","password":"p"}}]
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMockConfig(path); err == nil {
		t.Fatal("expected unknown tenant_id error")
	}
}

func TestAuthResponseShape(t *testing.T) {
	tenant := &mockTenant{
		TenantID: "acme",
		Provider: "oci",
		OCIEmail: &mockOCIEmail{Region: "us-phoenix-1", Username: "u", Password: "p"},
	}
	resp := tenant.authResponse()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatalf("invalid json: %s", b)
	}
}

func TestCheckRequiredHeaders(t *testing.T) {
	cfg := &mockConfig{
		RequiredHeaders: map[string]string{"X-Auth-Token": "local-dev"},
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/auth", nil)
	if err := cfg.checkRequiredHeaders(req); err == nil {
		t.Fatal("expected missing header error")
	}
	req.Header.Set("X-Auth-Token", "local-dev")
	if err := cfg.checkRequiredHeaders(req); err != nil {
		t.Fatalf("expected valid header: %v", err)
	}
}

func TestHandleAuthRejectsBadToken(t *testing.T) {
	cfg := &mockConfig{
		RequiredHeaders: map[string]string{"X-Auth-Token": "secret"},
		Users:           []mockUser{{Username: "u", Password: "p", TenantID: "acme"}},
		Tenants: []mockTenant{{
			TenantID: "acme", Provider: "oci",
			OCIEmail: &mockOCIEmail{Region: "r", Username: "u", Password: "p"},
		}},
	}
	s := httptest.NewServer(handleAuth(cfg))
	defer s.Close()

	resp, err := http.Post(s.URL, "application/json", strings.NewReader(`{"username":"u","password":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHandleAuthAcceptsValidCredentials(t *testing.T) {
	cfg := &mockConfig{
		RequiredHeaders: map[string]string{"X-Auth-Token": "secret"},
		Users:           []mockUser{{Username: "u", Password: "p", TenantID: "acme"}},
		Tenants: []mockTenant{{
			TenantID: "acme", Provider: "oci",
			OCIEmail: &mockOCIEmail{Region: "r", Username: "u", Password: "p"},
		}},
	}
	s := httptest.NewServer(handleAuth(cfg))
	defer s.Close()

	req, _ := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(`{"username":"u","password":"p"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
