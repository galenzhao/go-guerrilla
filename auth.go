package guerrilla

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/mail"
	"io"
	"net/http"
	"strings"
	"time"
)

// Authenticator validates SMTP AUTH credentials.
//
// For this implementation, only AUTH PLAIN is supported.
// identity is the optional "authorization identity" (may be empty).
type Authenticator interface {
	AuthenticatePlain(identity, username, password string, e *mail.Envelope) (AuthResult, error)
}

// AuthResult is the outcome of SMTP AUTH credential validation.
type AuthResult struct {
	OK     bool
	Tenant *backends.TenantSendConfig
}

type authenticatorHolder struct {
	Authenticator Authenticator
}

var errAuthPlainInvalid = errors.New("invalid AUTH PLAIN initial response")

// HTTPAuthenticatorConfig configures HTTP-based SMTP authentication.
type HTTPAuthenticatorConfig struct {
	URL     string
	Timeout time.Duration
	Headers map[string]string
}

// NewHTTPAuthenticatorFromConfig builds a HTTP authenticator from AppConfig.Auth.
func NewHTTPAuthenticatorFromConfig(c AuthConfig) (Authenticator, error) {
	if strings.TrimSpace(c.URL) == "" {
		return nil, errors.New("auth.url is required")
	}
	timeout := 2 * time.Second
	if strings.TrimSpace(c.Timeout) != "" {
		t, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return nil, errors.New("invalid auth.timeout")
		}
		timeout = t
	}
	return NewHTTPAuthenticator(HTTPAuthenticatorConfig{
		URL:     c.URL,
		Timeout: timeout,
		Headers: c.Headers,
	}), nil
}

type httpAuthenticator struct {
	url     string
	client  *http.Client
	headers map[string]string
}

type httpAuthRequest struct {
	Identity string `json:"identity,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
	RemoteIP string `json:"remote_ip,omitempty"`
	Helo     string `json:"helo,omitempty"`
	TLS      bool   `json:"tls"`
}

type httpAuthResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// NewHTTPAuthenticator creates an Authenticator that validates credentials via HTTP.
func NewHTTPAuthenticator(cfg HTTPAuthenticatorConfig) Authenticator {
	return &httpAuthenticator{
		url: cfg.URL,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		headers: cfg.Headers,
	}
}

func (h *httpAuthenticator) AuthenticatePlain(identity, username, password string, e *mail.Envelope) (AuthResult, error) {
	body, err := json.Marshal(httpAuthRequest{
		Identity: identity,
		Username: username,
		Password: password,
		RemoteIP: e.RemoteIP,
		Helo:     e.Helo,
		TLS:      e.TLS,
	})
	if err != nil {
		return AuthResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return AuthResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return AuthResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return AuthResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AuthResult{OK: false}, nil
	}
	var out httpAuthResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return AuthResult{}, err
	}
	if !out.OK {
		return AuthResult{OK: false}, nil
	}
	tenant, err := backends.ParseTenantSendConfigFromAuth(respBody)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{OK: true, Tenant: tenant}, nil
}

func decodeBase64Lenient(in string) ([]byte, error) {
	trimmed := strings.TrimSpace(in)
	// Most clients use StdEncoding (with padding), but some omit padding.
	if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(trimmed)
}

// parseAuthPlainIR parses AUTH PLAIN initial-response.
// The decoded payload is: [authzid] NUL authcid NUL passwd.
func parseAuthPlainIR(b64 string) (identity, username, password string, err error) {
	decoded, err := decodeBase64Lenient(b64)
	if err != nil {
		return "", "", "", errAuthPlainInvalid
	}
	// split by NUL
	parts := strings.Split(string(decoded), "\x00")
	// We expect at least: "", username, password  OR  identity, username, password
	if len(parts) < 3 {
		return "", "", "", errAuthPlainInvalid
	}
	// If there are more than 3 parts (extra NULs), keep the last 3 meaningful fields.
	identity = parts[len(parts)-3]
	username = parts[len(parts)-2]
	password = parts[len(parts)-1]
	if username == "" {
		return "", "", "", errAuthPlainInvalid
	}
	return identity, username, password, nil
}
