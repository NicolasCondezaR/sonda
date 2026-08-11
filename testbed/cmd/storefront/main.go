// Command storefront is the customer-facing service: a GraphQL endpoint, a
// WebSocket that pushes shelf updates, and a server-sent event stream.
//
// It is one service rather than three because that is what a storefront
// usually is, and because the three of them arrive in Sonda as three different
// kinds of capture from the same channel, which is worth seeing side by side.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/NicolasCondezaR/sonda/testbed/internal/toy"
	"github.com/NicolasCondezaR/sonda/testbed/internal/ws"
)

var flags = toy.NewFlags("graphql_down")

func main() {
	addr := flag.String("addr", ":8102", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	flags.Handle(mux)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		toy.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /graphql", graphqlHandler)
	mux.HandleFunc("GET /ws", socket)
	mux.HandleFunc("GET /events", events)

	toy.Listen(*addr, mux)
}

// request is one GraphQL operation as a client sends it.
type request struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
	Operation string         `json:"operationName,omitempty"`
}

// graphqlHandler answers the three documents this shop knows.
//
// It matches on the field name in the document instead of parsing GraphQL,
// which is all a toy needs — and it is the same shortcut Sonda takes when it
// labels a call, for the same reason.
func graphqlHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		toy.Fail(w, http.StatusBadRequest, "could not read body")
		return
	}

	if flags.On("graphql_down") {
		// A 502 with an HTML body: the failure that makes a GraphQL response
		// unreadable rather than merely wrong, which Sonda reports as
		// unreadable instead of as a call with no errors.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html><body><h1>502 Bad Gateway</h1></body></html>")
		return
	}

	// A batch is an array of operations in one POST. Clients really do this,
	// and a reader who only looked at the first would miss the rest.
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		var batch []request
		if err := json.Unmarshal(body, &batch); err != nil {
			toy.Fail(w, http.StatusBadRequest, "malformed GraphQL batch")
			return
		}
		out := make([]any, 0, len(batch))
		for _, req := range batch {
			out = append(out, resolve(req))
		}
		toy.JSON(w, http.StatusOK, out)
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		toy.Fail(w, http.StatusBadRequest, "malformed GraphQL request")
		return
	}
	toy.JSON(w, http.StatusOK, resolve(req))
}

// resolve answers one operation. Every answer is HTTP 200, including the
// failures — which is the whole point of the GraphQL exercise.
func resolve(req request) map[string]any {
	id, _ := req.Variables["id"].(string)

	switch {
	case strings.Contains(req.Query, "customer"):
		// The failure is selected by the request, not by a switch, so healthy
		// and failing GraphQL are in the field at the same time.
		if id == "ghost" {
			return map[string]any{
				"data": map[string]any{"customer": nil},
				"errors": []any{map[string]any{
					"message":    "no customer with that id",
					"path":       []any{"customer"},
					"extensions": map[string]any{"code": "NOT_FOUND"},
				}},
			}
		}
		return map[string]any{"data": map[string]any{
			"customer": map[string]any{
				"id":     id,
				"name":   "Ada Lovelace",
				"tier":   "member",
				"orders": []any{map[string]any{"id": "ORD-41", "total_cents": 1890}},
			},
		}}

	case strings.Contains(req.Query, "subscribe"):
		return map[string]any{"data": map[string]any{
			"subscribe": map[string]any{"ok": true},
		}}

	case strings.Contains(req.Query, "shelf"):
		return map[string]any{"data": map[string]any{
			"shelf": []any{"DUNE", "PALE-FIRE", "SOLARIS"},
		}}
	}

	return map[string]any{"errors": []any{map[string]any{
		"message":    "the storefront does not know that field",
		"extensions": map[string]any{"code": "GRAPHQL_VALIDATION_FAILED"},
	}}}
}

// socket holds a short conversation and then closes with a code and a reason.
//
// A client that says "boom" gets 1011 and an explanation; anyone else gets a
// clean 1000. The close frame is usually the only record of why a socket
// stopped working, which is why the test bed produces both kinds.
func socket(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Accept(w, r)
	if err != nil {
		slog.Error("websocket upgrade", "error", err)
		return
	}

	if err := conn.Text(`{"event":"welcome","shelf":"new arrivals"}`); err != nil {
		return
	}

	for {
		op, payload, err := conn.Read()
		if err != nil {
			conn.Hangup()
			return
		}
		switch op {
		case ws.OpClose:
			conn.CloseWith(ws.CloseNormal, "goodbye")
			return
		case ws.OpPing:
			continue
		}

		if strings.Contains(string(payload), "boom") {
			conn.CloseWith(ws.CloseInternal, "inventory feed lost, no shelf data")
			return
		}
		if err := conn.Text(`{"event":"stock","sku":"DUNE","on_hand":4}`); err != nil {
			return
		}
		if err := conn.Text(`{"event":"stock","sku":"SOLARIS","on_hand":0}`); err != nil {
			return
		}
	}
}

// events is an ordinary HTTP response that happens to be an event stream, which
// is exactly how Sonda treats it. ?broken=1 ends it with an error event.
func events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// A comment line. Sonda drops keepalives rather than showing them as
	// events, and there has to be one here for that to be demonstrable.
	fmt.Fprint(w, ": keep-alive\n\n")
	flusher.Flush()

	for i := 1; i <= 3; i++ {
		fmt.Fprintf(w, "event: shelf\nid: %d\ndata: {\"sku\":\"DUNE\",\"on_hand\":%d}\n\n", i, 5-i)
		flusher.Flush()
		time.Sleep(120 * time.Millisecond)
	}

	if r.URL.Query().Get("broken") == "1" {
		fmt.Fprint(w, "event: error\ndata: {\"message\":\"the shelf feed stopped\"}\n\n")
	} else {
		fmt.Fprint(w, "event: done\ndata: {\"sent\":3}\n\n")
	}
	flusher.Flush()
}
