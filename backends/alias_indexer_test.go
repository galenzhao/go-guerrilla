package backends

import "testing"

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
