package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	demov1 "github.com/NicolasCondezaR/sonda/examples/grpcdemo/demo/v1"
	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/fault"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/stub"
)

// These benchmarks measure what inserting Sonda costs, not how fast a request
// is. Only the difference between a Direct and its matching Proxied case means
// anything: an absolute figure here is mostly the loopback stack and the two
// httptest servers, and would say nothing about the proxy.
//
// Every pair is deliberately built the same way — same client, same payload,
// same loop — so the one thing that differs is the extra hop.
//
// The recorder is the real one, writing to a real SQLite file, because that is
// what ships. It is not on the latency path (Record is a non-blocking send onto
// a buffered channel and drops when full), but it allocates a copy of every
// captured body on the caller's goroutine and its drain goroutine competes for
// the same CPU, and both of those are costs a no-op sink would hide. The
// drops/op metric on the proxied cases says how much of the SQLite work the
// buffer absorbed instead of paying for.

const (
	benchSmallBody = 256
	benchLargeBody = 1 << 20

	// The shipped default, so the truncating case is the configuration a user
	// actually runs.
	benchDefaultMaxBody = 256 << 10
	benchDefaultBuffer  = 1024
)

func benchRecorder(b *testing.B) (*store.Recorder, *store.Store) {
	b.Helper()
	db, err := store.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	rec := store.NewRecorder(db, benchDefaultBuffer)
	ctx, cancel := context.WithCancel(context.Background())
	go rec.Run(ctx)
	b.Cleanup(func() {
		cancel()
		rec.Wait()
		db.Close()
	})
	return rec, db
}

// benchUpstream answers every request with the same body, reading the request
// in full first so a large POST is really transferred rather than reset.
func benchUpstream(b *testing.B, response []byte) *httptest.Server {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(response)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func benchProxy(b *testing.B, upstream string, maxBody int64, rec Recorder, stubs Stubs, faults Faults) *httptest.Server {
	b.Helper()
	target := config.Target{
		Name:     "bench",
		Listen:   "127.0.0.1:0",
		Upstream: upstream,
		Protocol: config.ProtocolHTTP,
	}
	front := httptest.NewServer(New(target, maxBody, rec, stubs, faults))
	b.Cleanup(front.Close)
	return front
}

// benchClient is a private client per benchmark: sharing http.DefaultClient
// would let one case warm the connection pool the next one measures.
func benchClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: tr}
}

// runHTTP is the whole measured loop, shared by both halves of every pair so a
// difference in the driver can never masquerade as a difference in the proxy.
// rec is nil for the direct case, which has no recorder to report on.
func runHTTP(b *testing.B, url string, body []byte, rec *store.Recorder) {
	client := benchClient()
	calls := 0

	b.ReportAllocs()
	for b.Loop() {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
		calls++
	}
	b.StopTimer()

	if rec != nil {
		b.ReportMetric(float64(rec.Dropped())/float64(calls), "drops/op")
	}
}

// A body small enough that the fixed per-call cost is all there is: the header
// clone, the two capture wrappers, the Call struct and the channel send.

func BenchmarkHTTPSmallDirect(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchSmallBody)
	upstream := benchUpstream(b, body)
	runHTTP(b, upstream.URL+"/things", body, nil)
}

func BenchmarkHTTPSmallProxied(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchSmallBody)
	upstream := benchUpstream(b, body)
	rec, _ := benchRecorder(b)
	front := benchProxy(b, upstream.URL, benchDefaultMaxBody, rec, nil, nil)
	runHTTP(b, front.URL+"/things", body, rec)
}

// bareReverseProxy is the middle term of every comparison: a stock
// httputil.ReverseProxy with none of Sonda in it, built out of the same two
// pieces Sonda uses so the gRPC case is compared against an HTTP/2 proxy rather
// than an HTTP/1 one.
//
// Without it, the whole Proxied-minus-Direct difference reads as "what capture
// costs", when most of it is the price of a second TCP hop and of ReverseProxy
// itself — which is what any proxy costs, Sonda or not. Sonda's own share is
// Proxied minus this.
func bareReverseProxy(b *testing.B, upstream string, grpcTarget bool) *httptest.Server {
	b.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		b.Fatal(err)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target); r.SetXForwarded() },
	}

	// The baseline enables full duplex because Sonda does, and the comparison is
	// only fair between two proxies doing the same job. Leaving it off does not
	// make the baseline cheaper — it makes it wrong: this benchmark was written
	// without it and the large-body case failed with "unexpected EOF" on the
	// first full run, which is the truncation bug fixed in ServeHTTP,
	// reproduced here with no Sonda code in the path at all.
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		rp.ServeHTTP(w, r)
	})
	if grpcTarget {
		rp.Transport = h2cTransport()
		handler = h2cHandler(rp)
	}
	front := httptest.NewServer(handler)
	b.Cleanup(front.Close)
	return front
}

func BenchmarkHTTPSmallBareReverseProxy(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchSmallBody)
	upstream := benchUpstream(b, body)
	runHTTP(b, bareReverseProxy(b, upstream.URL, false).URL+"/things", body, nil)
}

// The ordinary configuration: both registries wired in with no rule matching
// this target. Nothing is stubbed and nothing is broken, but the lookup happens
// on every single call, so it is worth knowing what an empty one costs.

func BenchmarkHTTPSmallProxiedWithRegistries(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchSmallBody)
	upstream := benchUpstream(b, body)
	rec, db := benchRecorder(b)
	front := benchProxy(b, upstream.URL, benchDefaultMaxBody, rec, stub.New(db), fault.New())
	runHTTP(b, front.URL+"/things", body, rec)
}

// discardRecorder answers the question "how much of the cost is the recorder?"
// and nothing else. It excludes the copy of the captured body handed to the
// sink, the channel send, the SQLite insert and the drain goroutine's share of
// the CPU — so it is not a figure for what Sonda costs. Only the difference
// between it and BenchmarkHTTPSmallProxied is meaningful.
type discardRecorder struct{}

func (discardRecorder) Record(*store.Call) {}

func BenchmarkHTTPSmallProxiedNoRecorder(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchSmallBody)
	upstream := benchUpstream(b, body)
	front := benchProxy(b, upstream.URL, benchDefaultMaxBody, discardRecorder{}, nil, nil)
	runHTTP(b, front.URL+"/things", body, nil)
}

// A body big enough that copying dominates. Two variants, because truncation
// changes what capture does: under the cap the whole megabyte is buffered and
// then copied again for the recorder; over it, the copy stops at the cap and
// only the byte counting continues.

func BenchmarkHTTPLargeDirect(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchLargeBody)
	upstream := benchUpstream(b, body)
	runHTTP(b, upstream.URL+"/upload", body, nil)
}

func BenchmarkHTTPLargeBareReverseProxy(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchLargeBody)
	upstream := benchUpstream(b, body)
	runHTTP(b, bareReverseProxy(b, upstream.URL, false).URL+"/upload", body, nil)
}

func BenchmarkHTTPLargeProxiedCaptured(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchLargeBody)
	upstream := benchUpstream(b, body)
	rec, _ := benchRecorder(b)
	front := benchProxy(b, upstream.URL, 2*benchLargeBody, rec, nil, nil)
	runHTTP(b, front.URL+"/upload", body, rec)
}

func BenchmarkHTTPLargeProxiedTruncated(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchLargeBody)
	upstream := benchUpstream(b, body)
	rec, _ := benchRecorder(b)
	front := benchProxy(b, upstream.URL, benchDefaultMaxBody, rec, nil, nil)
	runHTTP(b, front.URL+"/upload", body, rec)
}

// This one exists to check the harness, not the proxy. A 2ms injected delay is
// three orders of magnitude above the overhead being measured, so if the
// proxied-minus-direct difference does not grow by roughly 2ms here, the loop
// is measuring something other than time spent inside Sonda and none of the
// figures above can be trusted.
func BenchmarkHTTPSmallProxiedDelayed2ms(b *testing.B) {
	body := bytes.Repeat([]byte("x"), benchSmallBody)
	upstream := benchUpstream(b, body)
	rec, _ := benchRecorder(b)

	faults := fault.New()
	if err := faults.Set("bench", fault.Rule{LatencyMS: 2, OneIn: 1}); err != nil {
		b.Fatal(err)
	}
	front := benchProxy(b, upstream.URL, benchDefaultMaxBody, rec, nil, faults)
	runHTTP(b, front.URL+"/things", body, rec)
}

// gRPC goes through the same ServeHTTP, but over HTTP/2 with an h2c wrapper on
// the way in and a second HTTP/2 stack on the way out, which is a different
// fixed cost from the HTTP/1 case above.

func benchGRPCClient(b *testing.B, addr string) demov1.OrdersClient {
	b.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { conn.Close() })
	return demov1.NewOrdersClient(conn)
}

func benchGRPCProxied(b *testing.B, upstreamAddr string) (demov1.OrdersClient, *store.Recorder) {
	b.Helper()
	rec, _ := benchRecorder(b)
	target := config.Target{
		Name:     "bench",
		Listen:   "127.0.0.1:0",
		Upstream: "http://" + upstreamAddr,
		Protocol: config.ProtocolGRPC,
	}
	front := httptest.NewServer(New(target, benchDefaultMaxBody, rec, nil, nil).Handler())
	b.Cleanup(front.Close)
	return benchGRPCClient(b, front.Listener.Addr().String()), rec
}

func runUnary(b *testing.B, client demov1.OrdersClient, rec *store.Recorder) {
	ctx := context.Background()
	calls := 0

	b.ReportAllocs()
	for b.Loop() {
		if _, err := client.GetOrder(ctx, &demov1.GetOrderRequest{OrderId: "ORD-042"}); err != nil {
			b.Fatal(err)
		}
		calls++
	}
	b.StopTimer()

	if rec != nil {
		b.ReportMetric(float64(rec.Dropped())/float64(calls), "drops/op")
	}
}

func BenchmarkGRPCUnaryDirect(b *testing.B) {
	runUnary(b, benchGRPCClient(b, startUpstream(b)), nil)
}

func BenchmarkGRPCUnaryProxied(b *testing.B) {
	client, rec := benchGRPCProxied(b, startUpstream(b))
	runUnary(b, client, rec)
}

func BenchmarkGRPCUnaryBareReverseProxy(b *testing.B) {
	front := bareReverseProxy(b, "http://"+startUpstream(b), true)
	runUnary(b, benchGRPCClient(b, front.Listener.Addr().String()), nil)
}

// Server streaming is not a throughput question. Total time per stream is
// dominated by whatever the server waits between messages, so ns/op would move
// with the sleep and not with the proxy. What matters is whether a message is
// held up on its way through, which is ns/msg-lag: how long after the moment
// the server sent a message the client saw it. A proxy that buffered the stream
// would show that number climbing message by message.
//
// Only the difference between the two cases means anything, and rather more so
// here than elsewhere: the absolute lag is inflated by the server's own sleep
// overshooting on Windows, which the arithmetic charges to transit. Both cases
// run against the same server, so that inflation cancels in the delta.
func runServerStream(b *testing.B, client demov1.OrdersClient) {
	const gap = 2 * time.Millisecond
	const messages = 5

	ctx := context.Background()
	var lag time.Duration
	received := 0

	b.ReportAllocs()
	for b.Loop() {
		started := time.Now()
		stream, err := client.ListOrders(ctx, &demov1.ListOrdersRequest{
			Limit:   messages,
			DelayMs: int32(gap.Milliseconds()),
		})
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; ; i++ {
			if _, err := stream.Recv(); err == io.EOF {
				break
			} else if err != nil {
				b.Fatal(err)
			}
			// The server sends message i at roughly i*gap after the call starts,
			// so anything beyond that is time the message spent in transit.
			lag += time.Since(started) - time.Duration(i)*gap
			received++
		}
	}
	b.StopTimer()

	b.ReportMetric(float64(lag.Nanoseconds())/float64(received), "ns/msg-lag")
}

func BenchmarkGRPCServerStreamDirect(b *testing.B) {
	runServerStream(b, benchGRPCClient(b, startUpstream(b)))
}

func BenchmarkGRPCServerStreamProxied(b *testing.B) {
	client, _ := benchGRPCProxied(b, startUpstream(b))
	runServerStream(b, client)
}
