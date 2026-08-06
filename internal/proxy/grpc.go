package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"mirador/internal/config"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// h2cTransport speaks HTTP/2 without TLS. Local gRPC services are almost never
// behind TLS, and net/http's own transport only negotiates HTTP/2 over TLS, so
// this is the piece that makes plaintext gRPC forwarding possible at all.
func h2cTransport() http.RoundTripper {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}

// h2cHandler accepts cleartext HTTP/2, both by prior knowledge (what gRPC
// clients do) and through an HTTP/1.1 upgrade.
func h2cHandler(h http.Handler) http.Handler {
	return h2c.NewHandler(h, &http2.Server{})
}

// ReplayHeader marks a request Mirador is sending on the operator's behalf.
// The proxy strips it before forwarding, so the upstream receives exactly the
// original request and the captured headers do not gain a field the real client
// never sent — the replay is recorded as a link, not as a difference in the
// traffic.
const ReplayHeader = "X-Mirador-Replay-Of"

// replayedFrom reads and removes the marker.
func replayedFrom(h http.Header) *int64 {
	raw := h.Get(ReplayHeader)
	if raw == "" {
		return nil
	}
	h.Del(ReplayHeader)
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

func isGRPCRequest(headers http.Header) bool {
	return strings.HasPrefix(headers.Get("Content-Type"), "application/grpc")
}

// explainUpstreamFailure turns the most likely configuration mistake into a
// sentence that says what to change.
//
// Pointing a grpc target at a plain HTTP/1.1 server is an easy slip — one wrong
// port — and the raw error for it ("frame too large, note that the frame header
// looked like an HTTP/1.1 header") explains nothing to someone who did not
// write an HTTP/2 stack. Matching on the message is a heuristic; if the wording
// ever changes, the only loss is the friendlier hint.
func explainUpstreamFailure(target config.Target, upstream string, err error) string {
	if target.Protocol == config.ProtocolGRPC && strings.Contains(err.Error(), "looked like an HTTP/1.1 header") {
		return fmt.Sprintf(
			"mirador: target %q is configured as grpc, but %s answered HTTP/1.1. "+
				"Either point it at the gRPC port, or set protocol: http for this target. (%v)",
			target.Name, upstream, err)
	}
	return fmt.Sprintf("mirador: upstream %s unreachable: %v", upstream, err)
}

// grpcOutcome reads the real result of a gRPC call.
//
// The status does not travel in the HTTP status line — that is 200 even for a
// failure. It arrives in the trailers, after the body. The exception is a
// trailers-only response, which is how a server reports an error with nothing
// to send: then the status is in the headers and there are no trailers at all.
// Both have to be read, or half the failures worth debugging look like
// successes.
func grpcOutcome(resp *http.Response) (code int32, message string, ok bool) {
	raw := resp.Trailer.Get("Grpc-Status")
	source := resp.Trailer
	if raw == "" {
		raw = resp.Header.Get("Grpc-Status")
		source = resp.Header
	}
	if raw == "" {
		return 0, "", false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return 0, "", false
	}
	return int32(n), decodeGRPCMessage(source.Get("Grpc-Message")), true
}

// decodeGRPCMessage undoes the percent-encoding the gRPC spec applies to
// grpc-message, so an error reads as text instead of as %20-riddled noise.
// Anything malformed is returned as-is: a debugger showing the raw value beats
// one that hides it behind a decoding error.
func decodeGRPCMessage(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+2 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(byte(n))
		i += 2
	}
	return b.String()
}
