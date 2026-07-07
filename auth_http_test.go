package guerrilla

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flashmob/go-guerrilla/mail"
)

func TestHTTPAuthenticatorAuthenticatePlain(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()

	a := NewHTTPAuthenticator(HTTPAuthenticatorConfig{
		URL:     s.URL,
		Timeout: 2 * time.Second,
	})
	ok, err := a.AuthenticatePlain("", "u", "p", &mail.Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected authentication success")
	}
}

func TestHTTPAuthenticatorInvalidCredentials(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer s.Close()

	a := NewHTTPAuthenticator(HTTPAuthenticatorConfig{
		URL:     s.URL,
		Timeout: 2 * time.Second,
	})
	ok, err := a.AuthenticatePlain("", "u", "wrong", &mail.Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected authentication failure")
	}
}

func TestHTTPAuthenticatorTemporaryFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))
	defer s.Close()

	a := NewHTTPAuthenticator(HTTPAuthenticatorConfig{
		URL:     s.URL,
		Timeout: 2 * time.Second,
	})
	ok, err := a.AuthenticatePlain("", "u", "p", &mail.Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected non-2xx to fail authentication")
	}
}

func TestNewHTTPAuthenticatorFromConfig(t *testing.T) {
	_, err := NewHTTPAuthenticatorFromConfig(AuthConfig{Enabled: true, Type: "http"})
	if err == nil {
		t.Fatal("expected error when URL is empty")
	}

	a, err := NewHTTPAuthenticatorFromConfig(AuthConfig{
		Enabled: true,
		Type:    "http",
		URL:     "http://127.0.0.1:8080/auth",
		Timeout: "2s",
		Headers: map[string]string{"X-Test": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal(errors.New("expected authenticator"))
	}
}

