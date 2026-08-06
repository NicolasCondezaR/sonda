// Command echo is the toy upstream Mirador is developed and demoed against.
// It produces the situations worth capturing: a normal response, a slow one, a
// failure, and an echo of whatever was sent.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

func main() {
	addr := flag.String("addr", ":8081", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok",
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})

	// /slow?ms=1500 — for checking that Mirador reports duration and does not
	// cut a long call short.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		if ms <= 0 {
			ms = 1000
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"slept_ms": ms})
	})

	// /fail?status=503 — for checking that a failing upstream is captured with
	// its real status instead of Mirador's own error.
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.Atoi(r.URL.Query().Get("status"))
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]any{
			"error":  "the echo service failed on purpose",
			"status": status,
		})
	})

	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "could not read body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", valueOr(r.Header.Get("Content-Type"), "application/octet-stream"))
		w.WriteHeader(http.StatusOK)
		// Echoed byte for byte, so a Mirador capture can be compared against
		// what was actually sent.
		w.Write(body)
	})

	slog.Info("echo listening", "addr", *addr)
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		slog.Error("echo stopped", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
