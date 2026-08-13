package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type schemaUploadAPI struct {
	uploaded   int
	uploadPath string
}

func (a *schemaUploadAPI) Call(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	switch {
	case method == http.MethodGet && path == "/api/projects":
		return http.StatusOK, []byte(`{"projects":[{"id":3,"name":"core-delpagroup","services":[]}]}`), nil
	case method == http.MethodPost && strings.HasPrefix(path, "/api/projects/3/descriptor"):
		a.uploaded = len(body)
		a.uploadPath = path
		return http.StatusOK, []byte(fmt.Sprintf(`{"stored":%d,"services":1}`, len(body))), nil
	default:
		return http.StatusNotFound, []byte(`{"error":"no route in the fake for that"}`), nil
	}
}

func callStdioTool(t *testing.T, s *Server, name string, arguments map[string]any) (string, bool) {
	t.Helper()
	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), bytes.NewReader(append(request, '\n')), &out); err != nil {
		t.Fatal(err)
	}

	var answer response
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("the stdio answer is not JSON: %v", err)
	}
	if answer.Error != nil {
		t.Fatalf("protocol error: %s", answer.Error.Message)
	}
	result, ok := answer.Result.(map[string]any)
	if !ok {
		t.Fatalf("tool result has type %T", answer.Result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("tool result has no text content: %#v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	isError, _ := result["isError"].(bool)
	return text, isError
}

// One message too large used to end the process. bufio.Scanner returns
// ErrTooLong for good — every message after it is unreadable — ServeStdio
// returned it and `sonda mcp` exited, so an upload_schemas of about three
// megabytes (four thirds of that once base64-encoded) did not fail: the client
// watched the server die and had no idea why.
func TestAnOversizedMessageIsRefusedAndTheSessionSurvives(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_schemas","arguments":{"content_base64":"` +
		strings.Repeat("A", maxMessage+1024) + `"}}}`
	in := strings.NewReader(huge + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")

	var out bytes.Buffer
	s := New(&routedAPI{}, "test")
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("the server stopped on an oversized message: %v", err)
	}

	var answers []response
	decoder := json.NewDecoder(&out)
	for decoder.More() {
		var r response
		if err := decoder.Decode(&r); err != nil {
			t.Fatal(err)
		}
		answers = append(answers, r)
	}
	if len(answers) != 2 {
		t.Fatalf("%d answers, want the refusal and the message after it", len(answers))
	}

	if answers[0].Error == nil {
		t.Fatalf("the oversized message was not reported at all: %+v", answers[0])
	}
	// It has to say what to do instead, or the agent retries the same upload.
	if !strings.Contains(answers[0].Error.Message, "path") {
		t.Errorf("the refusal does not say how to send something this large: %q", answers[0].Error.Message)
	}
	if answers[1].Error != nil {
		t.Errorf("the message after the oversized one was not answered: %+v", answers[1])
	}
}

// A message that fits still has to be read whole, and 64 KB is nothing for a
// captured body quoted back into a tool result.
func TestALargeButLegalMessageIsStillRead(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_calls","arguments":{"text":"` +
		strings.Repeat("x", 512<<10) + `"}}}`

	var out bytes.Buffer
	s := New(&routedAPI{routes: map[string]string{"GET /api/calls": `{"calls":[]}`}}, "test")
	if err := s.ServeStdio(context.Background(), strings.NewReader(big+"\n"), &out); err != nil {
		t.Fatal(err)
	}

	var answer response
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not one JSON message: %v", err)
	}
	if answer.Error != nil {
		t.Errorf("a message well under the limit was refused: %v", answer.Error.Message)
	}
}

// A last line with no newline is still a message: a client that writes one
// request and closes the pipe must not be ignored.
func TestTheLastLineWithoutANewlineIsAnswered(t *testing.T) {
	var out bytes.Buffer
	s := New(&routedAPI{}, "test")
	if err := s.ServeStdio(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Error("a request that arrived without a trailing newline got no answer")
	}
}

// A descriptor larger than the whole stdio message budget cannot travel as
// base64 JSON. The local adapter reads the file before making the API call, so
// the JSON request stays small while the original bytes reach Sonda unchanged.
func TestStdioUploadsALargeDescriptorFromALocalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delpa.binpb")
	raw := bytes.Repeat([]byte{0x5a}, maxMessage+1024)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	api := &schemaUploadAPI{}
	text, isError := callStdioTool(t, NewStdio(api, "test"), "upload_schemas", map[string]any{
		"project": "core-delpagroup",
		"path":    path,
	})
	if isError {
		t.Fatalf("upload_schemas failed: %s", text)
	}
	if api.uploaded != len(raw) {
		t.Fatalf("uploaded %d bytes, want %d", api.uploaded, len(raw))
	}
	if !strings.Contains(api.uploadPath, "name=delpa.binpb") {
		t.Errorf("the path's base name was not kept as the descriptor name: %s", api.uploadPath)
	}
}

func TestStdioPathUploadReportsInvalidSources(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{
			name: "missing file",
			arguments: map[string]any{
				"project": "core-delpagroup",
				"path":    filepath.Join(t.TempDir(), "missing.binpb"),
			},
			want: "could not read descriptor set",
		},
		{
			name: "ambiguous sources",
			arguments: map[string]any{
				"project":        "core-delpagroup",
				"path":           "descriptor.binpb",
				"content_base64": "c29uZGE=",
			},
			want: "exactly one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &schemaUploadAPI{}
			text, isError := callStdioTool(t, NewStdio(api, "test"), "upload_schemas", tt.arguments)
			if !isError {
				t.Fatalf("invalid source was accepted: %s", text)
			}
			if !strings.Contains(text, tt.want) {
				t.Errorf("error %q does not contain %q", text, tt.want)
			}
			if api.uploaded != 0 {
				t.Errorf("uploaded %d bytes despite the source error", api.uploaded)
			}
		})
	}
}

// Even a stdio-capable Server must not read a path when the request arrived
// over HTTP. This guards the boundary independently of constructor wiring: a
// future refactor that accidentally reuses NewStdio for /mcp still cannot turn
// that remote endpoint into an arbitrary file reader.
func TestHTTPMCPNeverReadsALocalSchemaPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.binpb")
	if err := os.WriteFile(path, []byte("must not be read"), 0o600); err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "upload_schemas",
			"arguments": map[string]any{
				"project": "core-delpagroup",
				"path":    path,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	api := &schemaUploadAPI{}
	rec := httptest.NewRecorder()
	NewStdio(api, "test").Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(message)))

	var answer response
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	result, _ := answer.Result.(map[string]any)
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatalf("HTTP MCP accepted a local path: %s", rec.Body.String())
	}
	if api.uploaded != 0 {
		t.Errorf("HTTP MCP uploaded %d bytes from a local path", api.uploaded)
	}
	content, _ := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "only through the local stdio adapter") {
		t.Errorf("the refusal does not explain the transport boundary: %q", text)
	}
}
