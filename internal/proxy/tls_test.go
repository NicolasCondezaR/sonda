package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/NicolasCondezaR/sonda/internal/config"
)

// tlsUpstream is a real HTTPS server with a certificate nothing on this machine
// trusts, which is exactly the shape of the self-signed container the opt-out
// exists for.
func tlsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "from the service")
	}))
	t.Cleanup(s.Close)
	return s
}

func tlsTarget(upstream string, skip bool) config.Target {
	return config.Target{
		Name:               "test",
		Listen:             "127.0.0.1:0",
		Upstream:           upstream,
		Protocol:           config.ProtocolHTTP,
		InsecureSkipVerify: skip,
	}
}

// The default has to be the safe one, and it has to fail loudly. A proxy that
// quietly accepted any certificate would make every "upstream verified" reading
// in the tool a lie.
func TestAnUntrustedUpstreamIsRefusedUnlessAsked(t *testing.T) {
	upstream := tlsUpstream(t)
	rec := &collector{}

	front := httptest.NewServer(New(tlsTarget(upstream.URL, false), 1<<20, rec, nil, nil))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an unverifiable upstream answered %d, so its certificate was not checked", resp.StatusCode)
	}
	if !strings.Contains(string(body), "certificate") {
		t.Errorf("the 502 does not say why: %q", body)
	}

	call := rec.only(t)
	if call.Error == "" {
		t.Error("the capture does not record the transport failure")
	}
	if call.UpstreamInsecure {
		t.Error("the capture claims verification was skipped when it was not")
	}
}

func TestSkippingVerificationIsPerTargetAndRecorded(t *testing.T) {
	upstream := tlsUpstream(t)
	rec := &collector{}

	front := httptest.NewServer(New(tlsTarget(upstream.URL, true), 1<<20, rec, nil, nil))
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "from the service" {
		t.Fatalf("the opt-out did not let the call through: %d %q", resp.StatusCode, body)
	}

	call := rec.only(t)
	if !call.UpstreamTLS {
		t.Error("the capture does not say the upstream half was encrypted")
	}
	if !call.UpstreamInsecure {
		t.Error("the capture does not say the upstream was never verified — the one thing a reader must not have to go and look up")
	}

	// Per target, never global: turning it on for one service must leave the
	// transport every other service shares untouched.
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("the shared default transport now skips verification for every target in the process")
		}
	}
}

// The ordinary case, and the one the flag must not be needed for: an upstream
// whose certificate does verify goes through with the check on, and the capture
// says so.
func TestAVerifiableUpstreamNeedsNoOptOut(t *testing.T) {
	upstream := tlsUpstream(t)
	rec := &collector{}

	target := tlsTarget(upstream.URL, false)
	p := New(target, 1<<20, rec, nil, nil)
	// Trusting the test server's own certificate is what a developer does by
	// pointing SSL_CERT_FILE at a CA; the point is that verification is on.
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	p.reverse.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}

	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a verifiable upstream answered %d", resp.StatusCode)
	}

	call := rec.only(t)
	if !call.UpstreamTLS || call.UpstreamInsecure {
		t.Errorf("the capture reads upstream_tls=%v upstream_insecure=%v", call.UpstreamTLS, call.UpstreamInsecure)
	}
}

// A capture taken over a TLS listener has to say so, or the field shows an
// encrypted exchange as a plaintext one.
func TestATerminatedCallIsRecordedAsEncrypted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "plain upstream")
	}))
	defer upstream.Close()

	rec := &collector{}
	front := httptest.NewUnstartedServer(New(config.Target{
		Name: "test", Listen: "127.0.0.1:0", Upstream: upstream.URL, Protocol: config.ProtocolHTTP, TLS: true,
	}, 1<<20, rec, nil, nil))
	front.StartTLS()
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	call := rec.only(t)
	if !call.TLS {
		t.Error("a call that arrived over TLS is recorded as plaintext")
	}
	if call.UpstreamTLS {
		t.Error("terminating the client's TLS was mistaken for the upstream being encrypted")
	}
}

// A stub never leaves the machine, so reporting its upstream as verified — or
// as unverified — would be a reading of something that did not happen.
func TestAStubbedCallReportsNoUpstreamConnection(t *testing.T) {
	upstream := tlsUpstream(t)
	rec := &collector{}

	p := New(tlsTarget(upstream.URL, true), 1<<20, rec, nil, nil)
	front := httptest.NewServer(p)
	defer front.Close()

	p.recordStub(httptest.NewRequest(http.MethodGet, "/orders", nil), nil, nil,
		http.StatusNotImplemented, time.Now())

	call := rec.only(t)
	if call.UpstreamTLS || call.UpstreamInsecure {
		t.Errorf("a stubbed answer claims an upstream connection: tls=%v insecure=%v",
			call.UpstreamTLS, call.UpstreamInsecure)
	}
}

// The transport choice is where the two directions meet, and a gRPC target
// pointed at https was the one combination that silently dialled in the clear.
func TestTheTransportMatchesTheUpstreamScheme(t *testing.T) {
	// An ordinary http target keeps net/http's shared transport and its pool.
	if tr := upstreamTransport(config.Target{Protocol: config.ProtocolHTTP, Upstream: "http://x:1"}); tr != nil {
		t.Errorf("a plain http target was given its own transport: %T", tr)
	}
	// Both gRPC cases need HTTP/2, but only one of them may dial in the clear.
	plain, ok := upstreamTransport(config.Target{Protocol: config.ProtocolGRPC, Upstream: "http://x:1"}).(*http2.Transport)
	if !ok || !plain.AllowHTTP {
		t.Error("a cleartext gRPC target no longer gets h2c")
	}
	secure, ok := upstreamTransport(config.Target{Protocol: config.ProtocolGRPC, Upstream: "https://x:1"}).(*http2.Transport)
	if !ok || secure.AllowHTTP || secure.TLSClientConfig == nil {
		t.Error("a gRPC target behind TLS would be dialled in the clear")
	}
	if secure.TLSClientConfig.InsecureSkipVerify {
		t.Error("a gRPC target behind TLS skips verification without being asked")
	}

	// The dialled address has to carry the port the scheme implies, or a hosted
	// upstream written without one never connects.
	if got := upstreamAddr(config.Target{Upstream: "https://api.example.com"}); got != "api.example.com:443" {
		t.Errorf("https with no port dials %s", got)
	}
	if got := upstreamAddr(config.Target{Upstream: "http://api.example.com"}); got != "api.example.com:80" {
		t.Errorf("http with no port dials %s", got)
	}
	if got := upstreamAddr(config.Target{Upstream: "https://api.example.com:8443"}); got != "api.example.com:8443" {
		t.Errorf("an explicit port was rewritten to %s", got)
	}
}
