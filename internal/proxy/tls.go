package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// upstreamTransport picks how Sonda reaches the service.
//
// Three of the four cases already worked and one did not: a gRPC target has
// always been given h2cTransport, which dials in the clear by definition, so an
// https:// gRPC upstream was being spoken to in plaintext and failing. Keeping
// the choice in one function is what stops that pair drifting apart again.
//
// A nil result means "whatever net/http would have done", which is the right
// answer for the ordinary case and keeps DefaultTransport's connection pool
// shared across targets.
func upstreamTransport(t config.Target) http.RoundTripper {
	https := t.UpstreamURL().Scheme == "https"

	switch {
	case t.Protocol == config.ProtocolGRPC && https:
		// http2.Transport dials TLS itself and negotiates h2 over ALPN, which is
		// what a gRPC server behind TLS expects.
		return &http2.Transport{TLSClientConfig: clientConfig(t)}
	case t.Protocol == config.ProtocolGRPC:
		return h2cTransport()
	case t.InsecureSkipVerify:
		// Cloned rather than mutated: DefaultTransport is shared by every other
		// target in the process, and turning verification off on it would turn it
		// off for all of them — the global switch this feature deliberately is
		// not.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = clientConfig(t)
		return tr
	default:
		return nil
	}
}

// clientConfig is the only place InsecureSkipVerify is ever set, and it is set
// from one target's own configuration. There is no process-wide equivalent and
// there is not meant to be.
func clientConfig(t config.Target) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// #nosec G402 — the point of the flag. A developer's self-signed
		// container is the case this exists for; it is opt-in per target,
		// refused unless the upstream is https, shown on every surface that
		// shows the target, and recorded on every capture taken through it.
		InsecureSkipVerify: t.InsecureSkipVerify,
	}
}

// dialUpstream opens a raw connection to the service, encrypted when the
// upstream was declared https://. It is for the paths that hijack the
// connection and so never see the reverse proxy's transport.
func (p *Proxy) dialUpstream() (net.Conn, error) {
	addr := upstreamAddr(p.target)
	dialer := &net.Dialer{Timeout: upstreamDialTimeout}
	if !p.upstreamTLS {
		return dialer.Dial("tcp", addr)
	}
	cfg := clientConfig(p.target)
	// The name to check the certificate against is the one that was configured,
	// not the address it resolved to.
	cfg.ServerName = p.target.UpstreamURL().Hostname()
	return tls.DialWithDialer(dialer, "tcp", addr, cfg)
}

const upstreamDialTimeout = 10 * time.Second

// upstreamAddr fills in the port the scheme implies. net/http does this for the
// reverse proxy; a hand-rolled dial has to do it itself, and https://api.host
// with no port is the ordinary shape of a hosted upstream.
func upstreamAddr(t config.Target) string {
	u := t.UpstreamURL()
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// markTLS stamps a capture with how it was encrypted.
//
// forwarded says whether the upstream was actually reached: a stubbed answer and
// an injected failure never left the machine, so reporting their upstream as
// verified — or as unverified — would be a reading of something that did not
// happen.
func (p *Proxy) markTLS(call *store.Call, r *http.Request, forwarded bool) {
	call.TLS = r != nil && r.TLS != nil
	if !forwarded {
		return
	}
	call.UpstreamTLS = p.upstreamTLS
	call.UpstreamInsecure = p.upstreamTLS && p.target.InsecureSkipVerify
}
