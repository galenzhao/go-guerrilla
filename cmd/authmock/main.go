package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

var configPath string

func init() {
	flag.StringVar(&configPath, "c", "authmock.conf", "Path to authmock config file")
}

func main() {
	flag.Parse()

	cfg, err := loadMockConfig(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth", handleAuth(cfg))
	mux.HandleFunc("/tenants", handleTenants(cfg))

	log.Printf("auth mock listening on http://%s/auth and http://%s/tenants (config: %s)", cfg.Listen, cfg.Listen, configPath)
	log.Printf("loaded %d tenant(s), %d user(s)", len(cfg.Tenants), len(cfg.Users))
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

func handleAuth(cfg *mockConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("handled %s %s in %s", r.Method, r.URL.Path, time.Since(start))
		}()

		w.Header().Set("Content-Type", "application/json")

		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(authResponse{OK: false, Error: "method not allowed"})
			return
		}
		if err := cfg.checkRequiredHeaders(r); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(authResponse{OK: false, Error: err.Error()})
			return
		}

		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		fmt.Fprintln(os.Stdout, "---- AUTH MOCK REQUEST ----")
		fmt.Fprintf(os.Stdout, "%s %s\n", r.Method, r.URL.String())
		fmt.Fprintln(os.Stdout, "Headers:")
		for k, v := range r.Header {
			fmt.Fprintf(os.Stdout, "  %s: %v\n", k, v)
		}
		fmt.Fprintln(os.Stdout, "Body:")
		fmt.Fprintln(os.Stdout, string(body))
		fmt.Fprintln(os.Stdout, "---------------------------")

		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(authResponse{OK: false, Error: "invalid json"})
			return
		}

		user, ok := cfg.findUser(req.Username, req.Password)
		if !ok {
			_ = json.NewEncoder(w).Encode(authResponse{OK: false, Error: "invalid credentials"})
			return
		}

		tenant, ok := cfg.findTenant(user.TenantID)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(authResponse{OK: false, Error: "tenant not found"})
			return
		}

		_ = json.NewEncoder(w).Encode(tenant.authResponse())
	}
}

func handleTenants(cfg *mockConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("handled %s %s in %s", r.Method, r.URL.Path, time.Since(start))
		}()

		w.Header().Set("Content-Type", "application/json")

		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		if err := cfg.checkRequiredHeaders(r); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		_ = json.NewEncoder(w).Encode(tenantsResponse{Tenants: cfg.Tenants})
	}
}
