package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Before any service asks for TLS there is no authority, and the answer says so
// rather than inventing one. Creating a root because somebody opened a screen
// would be exactly the silent act this feature refuses to perform.
func TestNoAuthorityIsCreatedJustByAsking(t *testing.T) {
	h, _ := newServer(t)

	code, body := get(t, h, "/api/tls")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if exists, _ := body["exists"].(bool); exists {
		t.Error("an authority exists before any service asked for one")
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "trust store") {
		t.Errorf("the answer does not say that nothing is installed: %q", note)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tls/ca.pem", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("downloading a certificate that does not exist answered %d", rec.Code)
	}
}

// The answer has to be actionable from wherever the reader is. Every path in it
// is a path Sonda can see, which inside a container is a path nobody else can —
// so the certificate travels as bytes, and when Sonda can tell it is
// containerised it also says how to copy the file out.
//
// The key travels in neither case, and the assertions below are the guard: the
// whole encoded answer is searched for private key material, so a field added
// later that carried it would fail here rather than ship.
func TestTheCertificateIsReachableFromOutsideTheContainer(t *testing.T) {
	h := serverWithCA(t)

	// Not containerised: contents, no copy-out instruction to mislead with.
	code, body := get(t, h, "/api/tls")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	pem, _ := body["certificate_pem"].(string)
	if !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Fatalf("the answer carries no certificate to act on: %q", pem)
	}
	if strings.Contains(pem, "PRIVATE KEY") {
		t.Fatal("the certificate field carries key material")
	}
	if _, present := body["container"]; present {
		t.Error("a container hint was offered by a Sonda that is not in one")
	}

	// Containerised: the same bytes, plus the command that extracts the file.
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	previous := dockerEnvPath
	dockerEnvPath = marker
	t.Cleanup(func() { dockerEnvPath = previous })

	code, body = get(t, h, "/api/tls")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	hint, ok := body["container"].(map[string]any)
	if !ok {
		t.Fatalf("a containerised Sonda said nothing about it: %+v", body)
	}
	copyOut, _ := hint["copy_out"].(string)
	certPath := body["instructions"].(map[string]any)["path"].(string)
	if !strings.HasPrefix(copyOut, "docker cp ") || !strings.Contains(copyOut, certPath) {
		t.Errorf("the copy-out command does not extract the certificate: %q", copyOut)
	}

	// Whatever else was added, the key is in none of it.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("the API returned private key material")
	}
}

// Once one does exist, the API hands over the commands to trust it and to take
// it back out — and hands over the certificate, never the key.
func TestTheAuthorityIsPublishedWithoutItsKey(t *testing.T) {
	h := serverWithCA(t)

	code, body := get(t, h, "/api/tls")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if exists, _ := body["exists"].(bool); !exists {
		t.Fatal("the authority a TLS service forced into existence is not reported")
	}
	instructions, _ := body["instructions"].(map[string]any)
	for _, key := range []string{"per_tool", "trust_system_wide", "remove"} {
		if steps, _ := instructions[key].([]any); len(steps) == 0 {
			t.Errorf("the answer carries no %s steps", key)
		}
	}

	// Whatever else travels, the key does not.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PRIVATE KEY") {
		t.Fatal("the API returned private key material")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tls/ca.pem", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("downloading the certificate answered %d", rec.Code)
	}
	pem := rec.Body.String()
	if !strings.Contains(pem, "BEGIN CERTIFICATE") || strings.Contains(pem, "PRIVATE KEY") {
		t.Error("the download is not the public certificate alone")
	}

	// The same two facts have to reach the listing every interface reads, or a
	// service is unverified in the configuration and verified on screen.
	_, projects := get(t, h, "/api/projects")
	list, _ := projects["projects"].([]any)
	service := list[0].(map[string]any)["services"].([]any)[0].(map[string]any)
	if tlsOn, _ := service["tls"].(bool); !tlsOn {
		t.Error("the service listing does not say the port terminates TLS")
	}
	if skip, _ := service["insecure_skip_verify"].(bool); !skip {
		t.Error("the service listing does not say the upstream is unverified")
	}
	if at, _ := service["point_at"].(string); !strings.Contains(at, "https://") {
		t.Errorf("the line handed to the caller does not carry the scheme: %q", at)
	}
}

// serverWithCA is a Sonda with one TLS service, which is the only thing that
// makes an authority exist.
func serverWithCA(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	p, err := db.CreateProject(ctx, "secure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveService(ctx, store.Service{
		ProjectID: p.ID, Name: "api", Listen: "127.0.0.1:0",
		Upstream: "https://127.0.0.1:65000", Protocol: "http",
		TLS: true, InsecureSkipVerify: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}

	rt := runtime.New(db, noRecorder{}, 1<<20).WithCADir(dir)
	if err := rt.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	return New(db, noDrops{}, rt).Handler()
}

// A capture over TLS has to say so on the wire the interfaces read, or the
// field shows an unverified exchange as an ordinary one.
func TestCapturesCarryTheirEncryption(t *testing.T) {
	h, s := newServer(t)
	id, err := s.Insert(context.Background(), &store.Call{
		Target: "api", Protocol: "http", Method: "GET", Path: "/v1/orders",
		Status: 200, ClientAddr: "127.0.0.1:1", StartedAt: time.Now().UTC(),
		Duration: time.Millisecond,
		Request:  store.Message{Headers: http.Header{}},
		Response: store.Message{Headers: http.Header{}},
		TLS:      true, UpstreamTLS: true, UpstreamInsecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/calls", "/api/calls/" + strconv.FormatInt(id, 10)} {
		_, body := get(t, h, path)
		call := body
		if calls, ok := body["calls"].([]any); ok {
			call = calls[0].(map[string]any)
		}
		for _, key := range []string{"tls", "upstream_tls", "upstream_insecure"} {
			if flag, _ := call[key].(bool); !flag {
				t.Errorf("%s does not report %s", path, key)
			}
		}
	}
}
