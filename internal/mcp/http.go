package mcp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
)

// maxMessage bounds one request. MCP messages are small; anything larger is a
// mistake or an attack, and either way there is no reason to buffer it.
const maxMessage = 4 << 20

// Handler serves MCP over HTTP at a single endpoint.
//
// This is the transport that needs no installation: an agent is given a URL
// and that is all. It also means several agents can be pointed at the same
// running Sonda and see exactly the same captures, which a per-agent child
// process cannot guarantee on its own.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The specification requires this, and the reason is specific: a page
		// in the developer's own browser could otherwise POST here through
		// DNS rebinding and read every captured token. Absent is fine —
		// command-line clients do not send Origin — but present and foreign
		// is a 403, not a silent allow.
		if origin := r.Header.Get("Origin"); origin != "" && !isLocalOrigin(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}

		if r.Method != http.MethodPost {
			// GET opens the server-to-client stream in this transport, and
			// Sonda has no server-initiated messages to send. Saying so beats
			// leaving a stream open that will never carry anything.
			w.Header().Set("Allow", "POST")
			http.Error(w, "this endpoint takes POST", http.StatusMethodNotAllowed)
			return
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, maxMessage))
		if err != nil {
			writeRPC(w, http.StatusBadRequest, failure(nil, codeParse, "could not read the message"))
			return
		}

		resp := s.Handle(r.Context(), raw)
		if resp == nil {
			// A notification. Answering one is a protocol violation, so the
			// only correct reply is an empty acknowledgement.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, resp)
	})
}

func writeRPC(w http.ResponseWriter, status int, resp *response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
