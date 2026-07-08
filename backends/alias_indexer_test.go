package backends

import (
	"context"
	"testing"
	"time"
)

type stubPOP3Registry struct {
	accounts []TaggedPOP3Account
}

func (s *stubPOP3Registry) Refresh(context.Context) error { return nil }
func (s *stubPOP3Registry) All() []Tenant                 { return nil }
func (s *stubPOP3Registry) Get(string) (*Tenant, bool)    { return nil, false }
func (s *stubPOP3Registry) POP3Accounts() []TaggedPOP3Account {
	out := make([]TaggedPOP3Account, len(s.accounts))
	copy(out, s.accounts)
	return out
}
func (s *stubPOP3Registry) IMAPAccounts() []TaggedIMAPAccount { return nil }
func (s *stubPOP3Registry) PollInterval() time.Duration { return time.Minute }

func TestAliasIndexerIgnoresStaticAccountsWhenRegistrySet(t *testing.T) {
	reg := &stubPOP3Registry{
		accounts: []TaggedPOP3Account{{
			TenantID: "acme",
			Account: AliasPOP3Account{
				Host:     "pop.registry.example.com",
				Port:     995,
				TLS:      true,
				User:     "reg@acme.com",
				Password: "pw",
				TenantID: "acme",
			},
		}},
	}
	indexer := &AliasIndexer{
		cfg: AliasIndexerConfig{
			Accounts: []AliasPOP3Account{{
				Host: "pop.static.example.com",
				User: "static@example.com",
			}},
			Registry: reg,
		},
	}
	got := indexer.currentAccounts()
	if len(got) != 1 || got[0].Host != "pop.registry.example.com" {
		t.Fatalf("expected registry accounts only, got %+v", got)
	}

	// Empty registry must not fall back to static accounts.
	reg.accounts = nil
	got = indexer.currentAccounts()
	if len(got) != 0 {
		t.Fatalf("expected no fallback to static accounts, got %+v", got)
	}
}

func TestParseAliasPOP3AccountsList(t *testing.T) {
	cfg := BackendConfig{
		"alias_index_pop3_accounts": []interface{}{
			map[string]interface{}{
				"host":     "pop.a.example.com",
				"user":     "all@a.example.com",
				"password": "secret-a",
			},
			map[string]interface{}{
				"host":     "pop.b.example.com",
				"port":     float64(110),
				"tls":      false,
				"user":     "all@b.example.com",
				"password": "secret-b",
			},
		},
	}
	accounts, err := parseAliasPOP3Accounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0].MailboxKey() != "pop3:all@a.example.com@pop.a.example.com" {
		t.Fatalf("unexpected mailbox key: %s", accounts[0].MailboxKey())
	}
	if accounts[1].Port != 110 || accounts[1].TLS {
		t.Fatalf("unexpected second account: %+v", accounts[1])
	}
}

func TestParseLegacyAliasPOP3Account(t *testing.T) {
	cfg := BackendConfig{
		"alias_index_pop3_host":     "pop.exmail.qq.com",
		"alias_index_pop3_user":     "me@example.com",
		"alias_index_pop3_password": "pw",
	}
	accounts, err := parseAliasPOP3Accounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Host != "pop.exmail.qq.com" {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}
}

func TestParseAliasPOP3AccountsPrefersArray(t *testing.T) {
	cfg := BackendConfig{
		"alias_index_pop3_host": "legacy.example.com",
		"alias_index_pop3_user": "legacy@example.com",
		"alias_index_pop3_accounts": []interface{}{
			map[string]interface{}{
				"host": "pop.new.example.com",
				"user": "new@example.com",
			},
		},
	}
	accounts, err := parseAliasPOP3Accounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Host != "pop.new.example.com" {
		t.Fatalf("expected array config to win, got %+v", accounts)
	}
}
