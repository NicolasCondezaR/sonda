package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/http2"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/proxy"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

const replayTimeout = 60 * time.Second

// hopByHop must not be copied onto the replayed request: they describe the
// original connection, not the message.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Length":      true, // recomputed from the body being sent
	"Host":                true, // comes from the destination
}

type replayRequest struct {
	// Target sends the call to a different configured channel — the way to ask
	// "does this same request work against the other instance?". Empty replays
	// to the channel it came from.
	Target string `json:"target"`
}

type replayResponse struct {
	SentTo     string  `json:"sent_to"`
	Status     int     `json:"status"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

func (s *Server) replayCall(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a number")
		return
	}

	call, err := s.store.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no call with that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read call")
		return
	}

	// Neither a socket nor a Postgres statement is a request that can be sent
	// again: replaying one would open a new connection and label the result a
	// replay of this one — and a statement also belongs to a session and a
	// transaction that are gone, so even the same SQL would not be the same
	// call. Both clients already refuse to offer the control, but the refusal
	// belongs here, where every caller — including an agent over MCP — goes
	// through it.
	if call.Protocol == config.ProtocolWebSocket || call.Protocol == config.ProtocolPostgres {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"a %s capture cannot be replayed: it belongs to a connection that is gone, and sending it again would open a new one rather than repeat this one",
			call.Protocol))
		return
	}

	// A truncated capture holds only the head of the body. Sending it would put
	// a different request on the wire and label the result a replay, which is
	// the one thing this feature must never do. Refusing is the honest answer.
	if call.Request.Truncated {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"this request was %d bytes and only the first %d were stored, so it cannot be replayed faithfully. Raise max_body_bytes and capture it again.",
			call.Request.Size, len(call.Request.Body)))
		return
	}

	// An absent or empty body just means "replay where it came from".
	var body replayRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	targetName := body.Target
	if targetName == "" {
		targetName = call.Target
	}
	target, ok := s.target(targetName)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no channel named %q", targetName))
		return
	}

	result := s.send(r.Context(), call, target)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) target(name string) (config.Target, bool) {
	for _, t := range s.targets() {
		if t.Name == name {
			return t, true
		}
	}
	return config.Target{}, false
}

// send replays through Sonda's own listener rather than straight at the
// upstream. The replay is then captured like any other traffic: it appears in
// the field, and it can be diffed against the call it came from.
func (s *Server) send(ctx context.Context, call *store.Call, target config.Target) replayResponse {
	out := replayResponse{SentTo: target.Name}

	// A target with tls: true has its listener wrapped in a TLS one, so the port
	// rejects a cleartext request outright. The scheme is not a guess: it is the
	// same flag the supervisor read when it opened the socket.
	scheme := "http://"
	if target.TLS {
		scheme = "https://"
	}
	url := scheme + target.Listen + call.Path
	ctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, call.Method, url, bytes.NewReader(call.Request.Body))
	if err != nil {
		out.Error = err.Error()
		return out
	}

	for key, values := range call.Request.Headers {
		if hopByHop[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	// gRPC servers expect this and Go's proxy only forwards it when the inbound
	// request carried it.
	if call.Protocol == config.ProtocolGRPC {
		req.Header.Set("Te", "trailers")
	}
	req.Header.Set(proxy.ReplayHeader, strconv.FormatInt(call.ID, 10))

	started := time.Now()
	resp, err := replayClient(call.Protocol, target.TLS).Do(req)
	out.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer resp.Body.Close()
	// Drain so the capture on the proxy side sees the whole response before the
	// connection is released.
	_, _ = io.Copy(io.Discard, resp.Body)
	out.Status = resp.StatusCode
	return out
}

func replayClient(protocol string, listenerTLS bool) *http.Client {
	if protocol != config.ProtocolGRPC {
		if !listenerTLS {
			return &http.Client{}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: replayTLSConfig()}}
	}
	if listenerTLS {
		// No AllowHTTP and no dial override: http2.Transport does the handshake
		// itself and asks for h2 over ALPN, which is what the listener offers.
		return &http.Client{Transport: &http2.Transport{TLSClientConfig: replayTLSConfig()}}
	}
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// replayTLSConfig is for the one hop between Sonda and Sonda's own listener.
//
// The certificate on the other end was minted by an authority this process
// holds, for whatever name the connection asked for, so checking it would only
// be Sonda vouching for itself. Worse, it would fail on a target bound to
// 0.0.0.0 — the compose configuration binds every target that way — because the
// leaf is issued for the interface the connection landed on rather than for the
// address that was dialled. Nothing here leaves the machine, and the upstream
// hop beyond it is verified as it always was.
func replayTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- Sonda's own listener, see above
}
