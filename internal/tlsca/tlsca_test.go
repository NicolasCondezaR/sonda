package tlsca

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serve starts a real HTTPS listener terminating with the authority, and
// returns its address. Nothing here is simulated: every assertion below is the
// result of a handshake that actually happened.
func serve(t *testing.T, ca *CA) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "through")
	})}
	go server.Serve(tls.NewListener(ln, ca.Config()))
	t.Cleanup(func() { server.Close() })
	return ln.Addr().String()
}

// pool is what a client that has been told to trust the authority carries.
func pool(t *testing.T, ca *CA) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(ca.CertificatePEM()) {
		t.Fatal("the CA certificate is not a usable PEM")
	}
	return p
}

func openCA(t *testing.T, dir string) *CA {
	t.Helper()
	ca, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestATrustingClientCompletesTheHandshake(t *testing.T) {
	ca := openCA(t, t.TempDir())
	addr := serve(t, ca)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool(t, ca),
			ServerName: "example.test",
		}},
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("a client trusting the CA could not connect: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through" {
		t.Fatalf("got %q", body)
	}
}

// The other half of the contract: nothing is installed anywhere, so a client
// that was not told to trust the authority must refuse it. A test that only
// proves the trusting case would pass just as happily against a CA silently
// added to the machine's store.
func TestAnUntrustingClientIsRefused(t *testing.T) {
	ca := openCA(t, t.TempDir())
	addr := serve(t, ca)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + addr + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("a client with no knowledge of the CA accepted the certificate")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// The certificate has to name what the client asked for, or every client
// rejects it however well the CA is trusted.
func TestTheCertificateNamesWhatWasAskedFor(t *testing.T) {
	ca := openCA(t, t.TempDir())
	addr := serve(t, ca)
	roots := pool(t, ca)

	for _, name := range []string{"api.example.test", "another.test"} {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: name})
		if err != nil {
			t.Fatalf("handshake for %s: %v", name, err)
		}
		leaf := conn.ConnectionState().PeerCertificates[0]
		conn.Close()
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("the certificate served for %s does not cover it: %v", name, err)
		}
	}

	// No SNI at all is the ordinary case for `curl https://127.0.0.1:9443`, and
	// the address reached is then the only name there is.
	host, _, _ := net.SplitHostPort(addr)
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("handshake without SNI: %v", err)
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	if err := leaf.VerifyHostname(host); err != nil {
		t.Errorf("the certificate served to a client sending no SNI does not cover %s: %v", host, err)
	}
}

// A busy page opens many connections to one name. Issuing a key per connection
// would be both slow and pointless.
func TestOneCertificatePerName(t *testing.T) {
	ca := openCA(t, t.TempDir())
	addr := serve(t, ca)
	roots := pool(t, ca)

	serial := func() string {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: "cached.test"})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	if first, second := serial(), serial(); first != second {
		t.Errorf("a second connection to the same name got a freshly minted certificate: %s then %s", first, second)
	}
}

func TestTheKeyIsOwnerOnlyAndIsNotTheCertificate(t *testing.T) {
	dir := t.TempDir()
	openCA(t, dir)

	keyPath := filepath.Join(dir, keyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not carry POSIX permission bits at all, so asserting them
	// there would be asserting something Go invents.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the CA key is %s, not 0600", info.Mode().Perm())
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// The one thing that must never travel: whatever else is published, the
	// private key is not in it.
	published := string(openCA(t, dir).CertificatePEM()) + openCA(t, dir).Instructions().String()
	if strings.Contains(published, string(key)) || strings.Contains(published, "PRIVATE KEY-----\nM") {
		t.Error("the private key appears in something Sonda hands out")
	}
}

// Reopening must reuse the authority on disk. Minting a new one per run would
// mean the user re-trusting a root every time Sonda restarts, which is the
// fastest way to teach someone to click through certificate warnings.
func TestReopeningKeepsTheSameAuthority(t *testing.T) {
	dir := t.TempDir()
	first := openCA(t, dir).cert.SerialNumber.String()
	second := openCA(t, dir).cert.SerialNumber.String()
	if first != second {
		t.Errorf("a second Open minted a new authority: %s then %s", first, second)
	}
}

// A root good for a decade is a root nobody remembers installing.
func TestTheAuthorityExpires(t *testing.T) {
	ca := openCA(t, t.TempDir())
	if life := time.Until(ca.cert.NotAfter); life > 400*24*time.Hour {
		t.Errorf("the CA is valid for %v, which is not a bounded lifetime", life)
	}
	if !ca.cert.IsCA || !ca.cert.MaxPathLenZero {
		t.Error("the CA must be a CA and must not be able to mint another one")
	}
	if !strings.Contains(ca.cert.Subject.CommonName, "Sonda") {
		t.Errorf("the subject %q does not name the tool", ca.cert.Subject.CommonName)
	}
}

// Whatever else the instructions say, they have to say how to get rid of it.
func TestInstructionsCoverTrustAndRemoval(t *testing.T) {
	dir := t.TempDir()
	i := openCA(t, dir).Instructions()

	if i.Path != filepath.Join(dir, certFile) {
		t.Errorf("the instructions point at %s", i.Path)
	}
	for _, want := range []string{"macOS", "Windows", "Linux (Debian, Ubuntu)", "Firefox"} {
		if !contains(i.Trust, want) {
			t.Errorf("nothing tells a %s user how to trust it", want)
		}
		if !contains(i.Remove, want) {
			t.Errorf("nothing tells a %s user how to remove it", want)
		}
	}
	if !strings.Contains(i.String(), filepath.Join(dir, keyFile)) {
		t.Error("removal never mentions deleting the private key")
	}
}

func contains(steps []Step, where string) bool {
	for _, s := range steps {
		if s.Where == where && s.Command != "" {
			return true
		}
	}
	return false
}
