package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			log.Printf("handled %s %s in %s", r.Method, r.URL.Path, time.Since(start))
		}()

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

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	addr := "127.0.0.1:8080"
	log.Printf("auth mock listening on http://%s/auth", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

