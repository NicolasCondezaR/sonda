package supervisor

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/tlsca"
)

// authority uses the real one rather than a hand-rolled self-signed pair: the
// listener this package builds is the listener Sonda runs, and a test that
// swapped the certificate source would stop proving the two fit together.
func authority(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	ca, err := tlsca.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertificatePEM()) {
		t.Fatal("the CA certificate is not usable")
	}
	return ca.Config(), roots
}

func getTLS(t *testing.T, addr string, roots *x509.CertPool) (string, error) {
	t.Helper()
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}},
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func TestATLSListenerServesTheSameHandler(t *testing.T) {
	cfg, roots := authority(t)
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("encrypted"), TLS: cfg}})

	body, err := getTLS(t, addr, roots)
	if err != nil || body != "encrypted" {
		t.Fatalf("the TLS listener did not answer: %q %v", body, err)
	}
	// And it is genuinely a TLS port, not one that happens to accept both.
	// net/http answers a plaintext request to a TLS listener with a courtesy 400
	// rather than a connection error, so the check is that the handler was never
	// reached — not that the request failed to get a reply.
	if plain, _ := get(t, addr); plain == "encrypted" {
		t.Error("a plaintext request reached the handler on a listener configured for TLS")
	}
}

func TestATLSRawListenerHandsDecryptedBytesToServe(t *testing.T) {
	cfg, roots := authority(t)
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "rabbit", Listen: addr, TLS: cfg, Serve: func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err == nil && string(buf) == "ping" {
			_, _ = io.WriteString(c, "pong")
		}
	}}})

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "pong" {
		t.Fatalf("raw TLS reply = %q, %v", got, err)
	}
}

// The handler behind a listener is swapped in place while it runs, which is
// what keeps editing a service from closing its port. Encryption cannot be
// swapped that way: it belongs to the socket. If it were treated as swappable
// the port would keep answering in the clear while every interface reported it
// as encrypted, which is the one failure this package exists to prevent.
func TestTurningTLSOnAndOffRestartsTheListener(t *testing.T) {
	cfg, roots := authority(t)
	s := New()
	defer s.StopAll()

	addr := freePort(t)
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("plain")}})
	if body, err := get(t, addr); err != nil || body != "plain" {
		t.Fatalf("the plaintext listener did not answer: %q %v", body, err)
	}

	// Same key, same address, now encrypted.
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("encrypted"), TLS: cfg}})
	body, err := getTLS(t, addr, roots)
	if err != nil || body != "encrypted" {
		t.Fatalf("after turning TLS on the port did not speak it: %q %v", body, err)
	}
	if plain, _ := get(t, addr); plain == "plain" || plain == "encrypted" {
		t.Errorf("the port is still serving the handler in the clear after being switched to TLS: %q", plain)
	}

	// And back again, which is the direction that leaves a client trusting an
	// encryption that is no longer there.
	s.Apply([]Desired{{Key: "a", Listen: addr, Handler: echo("plain again")}})
	if body, err := get(t, addr); err != nil || body != "plain again" {
		t.Fatalf("after turning TLS off the port did not answer in the clear: %q %v", body, err)
	}
	if _, err := getTLS(t, addr, roots); err == nil {
		t.Error("the port is still completing TLS handshakes after being switched back")
	}
}
