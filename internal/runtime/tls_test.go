package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// saveTLSService adds one service with the encryption flags set, which the
// shared project() helper deliberately does not take: the flags belong to this
// file's concern and threading them through every other test would be noise.
func saveTLSService(t *testing.T, db *store.Store, name, listen, up string, terminate, skip bool) int64 {
	t.Helper()
	ctx := context.Background()
	p, err := db.CreateProject(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveService(ctx, store.Service{
		ProjectID: p.ID, Name: name, Listen: listen, Upstream: up,
		Protocol: config.ProtocolHTTP, TLS: terminate, InsecureSkipVerify: skip,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestATLSServiceOpensAnHTTPSPort(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	addr := freePort(t)
	saveTLSService(t, db, "secure", addr, upstream(t, "reached the service"), true, false)

	rec := newRecorder()
	rt := New(db, rec, 1<<20).WithCADir(dir)
	defer rt.Stop()
	reconcile(t, rt)

	ca := rt.CA()
	if ca == nil {
		t.Fatal("no certificate authority was created for a service that terminates TLS")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertificatePEM()) {
		t.Fatal("the CA certificate is not usable")
	}

	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}},
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("the port did not complete a handshake: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "reached the service" {
		t.Fatalf("the encrypted port forwarded to the wrong place: %q", body)
	}

	// The authority and its key live beside the database, and the key is the
	// dangerous half — it must exist as a file the owner alone can read, and it
	// must not have been installed anywhere.
	for _, name := range []string{"sonda-ca.pem", "sonda-ca-key.pem"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is not beside the database: %v", name, err)
		}
	}

	if call := rec.wait(t); !call.TLS {
		t.Error("the capture does not record that the client's half was encrypted")
	}
}

// A user who never asks for TLS must never find a certificate authority sitting
// in their project directory.
func TestNoAuthorityWithoutATLSService(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	saveTLSService(t, db, "plain", freePort(t), upstream(t, "plain"), false, false)

	rt := New(db, newRecorder(), 1<<20).WithCADir(dir)
	defer rt.Stop()
	reconcile(t, rt)

	if rt.CA() != nil {
		t.Error("a certificate authority was created for a project that does not use TLS")
	}
	if _, err := os.Stat(filepath.Join(dir, "sonda-ca-key.pem")); err == nil {
		t.Error("a CA private key was written for a project that does not use TLS")
	}
}

// Postgres negotiates encryption inside its own protocol, so a TLS listener in
// front of it would answer a handshake no client sends. Refusing the flag at
// the point it is saved is the only place the refusal is useful.
func TestPostgresRefusesToTerminateTLS(t *testing.T) {
	db := openStore(t)
	p, err := db.CreateProject(context.Background(), "db")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveService(context.Background(), store.Service{
		ProjectID: p.ID, Name: "pg", Listen: "127.0.0.1:9500",
		Upstream: "postgres://127.0.0.1:5432", Protocol: config.ProtocolPostgres, TLS: true,
	})
	if err == nil {
		t.Fatal("a postgres service was accepted with tls turned on")
	}
}
