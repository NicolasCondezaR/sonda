package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	demov1 "mirador/examples/grpcdemo/demo/v1"
	"mirador/examples/grpcdemo/ordersvc"
	"mirador/internal/config"
	"mirador/internal/grpcwire"
)

// startUpstream runs the demo gRPC service and returns its address.
func startUpstream(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	ordersvc.Register(srv, true)
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	return listener.Addr().String()
}

// startGRPCProxy puts Mirador in front of an upstream and returns a connected
// client plus the recorder holding whatever was captured.
func startGRPCProxy(t *testing.T, upstreamAddr string) (demov1.OrdersClient, *collector) {
	t.Helper()
	rec := &collector{}
	target := config.Target{
		Name:     "orders",
		Listen:   "127.0.0.1:0",
		Upstream: "http://" + upstreamAddr,
		Protocol: config.ProtocolGRPC,
	}
	front := httptest.NewServer(New(target, 1<<20, rec).Handler())
	t.Cleanup(front.Close)

	conn, err := grpc.NewClient(front.Listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return demov1.NewOrdersClient(conn), rec
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// This is the test the whole phase rests on.
//
// A gRPC call reports its outcome in HTTP/2 trailers, which arrive after the
// body. A proxy that drops them makes every single call look failed to the
// client, no matter how well the rest works. Nothing else in this file matters
// if this does not pass.
func TestGRPCTrailersSurviveTheProxy(t *testing.T) {
	client, _ := startGRPCProxy(t, startUpstream(t))

	order, err := client.GetOrder(ctx(t), &demov1.GetOrderRequest{OrderId: "ORD-042"})
	if err != nil {
		t.Fatalf("a successful call came back as an error, which is what losing trailers looks like: %v", err)
	}
	if order.GetId() != "ORD-042" {
		t.Errorf("order id = %q, want ORD-042", order.GetId())
	}
	if len(order.GetLines()) != 2 {
		t.Errorf("got %d lines, want 2", len(order.GetLines()))
	}
}

// The other half of the same problem: a real failure has to stay a failure,
// with its own code, instead of being flattened into a generic error.
func TestGRPCErrorStatusReachesTheClient(t *testing.T) {
	client, rec := startGRPCProxy(t, startUpstream(t))

	_, err := client.Fail(ctx(t), &demov1.FailRequest{
		Code:    int32(codes.PermissionDenied),
		Message: "no tienes acceso a este pedido",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status: %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", st.Code())
	}
	// The message is percent-encoded on the wire; the client decodes it, and so
	// must the capture.
	if st.Message() != "no tienes acceso a este pedido" {
		t.Errorf("message = %q", st.Message())
	}

	call := rec.only(t)
	if call.GRPCStatus == nil {
		t.Fatal("the capture has no gRPC status")
	}
	if *call.GRPCStatus != int32(codes.PermissionDenied) {
		t.Errorf("captured status = %d, want %d", *call.GRPCStatus, codes.PermissionDenied)
	}
	if call.GRPCMessage != "no tienes acceso a este pedido" {
		t.Errorf("captured message = %q, want it percent-decoded", call.GRPCMessage)
	}
	// A failed gRPC call still carries HTTP 200. Recording only the HTTP status
	// would file this as a success.
	if call.Status != 200 {
		t.Errorf("http status = %d, want 200 — gRPC reports failure below HTTP", call.Status)
	}
}

func TestGRPCUnaryCallIsCaptured(t *testing.T) {
	client, rec := startGRPCProxy(t, startUpstream(t))

	if _, err := client.GetOrder(ctx(t), &demov1.GetOrderRequest{OrderId: "ORD-007"}); err != nil {
		t.Fatal(err)
	}

	call := rec.only(t)
	if call.Protocol != config.ProtocolGRPC {
		t.Errorf("protocol = %q, want grpc", call.Protocol)
	}
	if call.Path != "/demo.v1.Orders/GetOrder" {
		t.Errorf("path = %q", call.Path)
	}
	if call.GRPCStatus == nil || *call.GRPCStatus != 0 {
		t.Errorf("grpc status = %v, want 0 (OK)", call.GRPCStatus)
	}

	service, method, ok := grpcwire.MethodParts(call.Path)
	if !ok || service != "demo.v1.Orders" || method != "GetOrder" {
		t.Errorf("method parts = %q %q %v", service, method, ok)
	}

	// The stored bytes are the framed stream, so they have to deframe back
	// into exactly the messages that crossed.
	requests, remainder := grpcwire.Deframe(call.Request.Body)
	if len(requests) != 1 || remainder != 0 {
		t.Fatalf("request deframed to %d messages with %d bytes left over", len(requests), remainder)
	}
	responses, remainder := grpcwire.Deframe(call.Response.Body)
	if len(responses) != 1 || remainder != 0 {
		t.Fatalf("response deframed to %d messages with %d bytes left over", len(responses), remainder)
	}
}

// Streaming is where a one-request-one-response model breaks. Every message has
// to be captured, not just the first.
func TestGRPCServerStreamingCapturesEveryMessage(t *testing.T) {
	client, rec := startGRPCProxy(t, startUpstream(t))

	stream, err := client.ListOrders(ctx(t), &demov1.ListOrdersRequest{Limit: 5, Customer: "Andes"})
	if err != nil {
		t.Fatal(err)
	}
	received := 0
	for {
		order, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("after %d messages: %v", received, err)
		}
		if order.GetCustomer() != "Andes" {
			t.Errorf("customer = %q", order.GetCustomer())
		}
		received++
	}
	if received != 5 {
		t.Fatalf("client received %d messages, want 5", received)
	}

	call := rec.only(t)
	responses, remainder := grpcwire.Deframe(call.Response.Body)
	if len(responses) != 5 {
		t.Errorf("captured %d response messages, want 5", len(responses))
	}
	if remainder != 0 {
		t.Errorf("%d bytes left over after the last message", remainder)
	}
	if call.GRPCStatus == nil || *call.GRPCStatus != 0 {
		t.Errorf("grpc status = %v, want 0", call.GRPCStatus)
	}
}

// Capturing every message of a stream is not the same as delivering them as
// they happen. A proxy that buffers the whole stream and hands it over at the
// end still passes a count assertion while destroying the timing the developer
// opened this tool to see — and buffering is the default behaviour that has to
// be actively avoided.
func TestGRPCServerStreamIsNotBuffered(t *testing.T) {
	client, _ := startGRPCProxy(t, startUpstream(t))

	const gap = 120 * time.Millisecond
	stream, err := client.ListOrders(ctx(t), &demov1.ListOrdersRequest{Limit: 3, DelayMs: int32(gap.Milliseconds())})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	var arrivals []time.Duration
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		arrivals = append(arrivals, time.Since(started))
	}

	if len(arrivals) != 3 {
		t.Fatalf("received %d messages, want 3", len(arrivals))
	}
	// The first must land well before the last is even sent. Buffering would
	// bunch all three at the end.
	if arrivals[0] > gap {
		t.Errorf("first message took %v, but the server sent it immediately — the proxy buffered", arrivals[0])
	}
	if arrivals[2]-arrivals[0] < gap {
		t.Errorf("all three arrived within %v of each other; they were sent %v apart",
			arrivals[2]-arrivals[0], gap)
	}
}

func TestGRPCClientStreamingCapturesEveryMessage(t *testing.T) {
	client, rec := startGRPCProxy(t, startUpstream(t))

	stream, err := client.CreateOrders(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := stream.Send(ordersvc.SampleOrder(string(rune('A' + i)))); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if summary.GetCreated() != 4 {
		t.Errorf("server counted %d orders, want 4", summary.GetCreated())
	}

	call := rec.only(t)
	requests, remainder := grpcwire.Deframe(call.Request.Body)
	if len(requests) != 4 {
		t.Errorf("captured %d request messages, want 4", len(requests))
	}
	if remainder != 0 {
		t.Errorf("%d bytes left over after the last message", remainder)
	}
}

// A gRPC port carries more than gRPC — health and metrics endpoints usually sit
// on the same listener — and those are HTTP/2 too, since that is what the port
// speaks. The protocol recorded for them must come from the content type, not
// from how the target was configured.
func TestGRPCTargetClassifiesPlainHTTPByContentType(t *testing.T) {
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(plainOK{}, &http2.Server{}))
	upstream.Start()
	defer upstream.Close()

	rec := &collector{}
	target := config.Target{
		Name:     "mixed",
		Listen:   "127.0.0.1:0",
		Upstream: upstream.URL,
		Protocol: config.ProtocolGRPC,
	}
	front := httptest.NewServer(New(target, 1024, rec).Handler())
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
	call := rec.only(t)
	if call.Protocol != config.ProtocolHTTP {
		t.Errorf("protocol = %q, want http — the content type decides, not the target", call.Protocol)
	}
	if call.GRPCStatus != nil {
		t.Error("a plain HTTP call should carry no gRPC status")
	}
}

// Pointing a grpc target at an HTTP/1.1-only port is one wrong digit away, and
// the raw HTTP/2 framing error explains nothing. The reply has to name the fix.
func TestGRPCTargetPointedAtAnHTTP1UpstreamSaysSo(t *testing.T) {
	upstream := httptest.NewServer(plainOK{})
	defer upstream.Close()

	rec := &collector{}
	target := config.Target{
		Name:     "wrong-port",
		Listen:   "127.0.0.1:0",
		Upstream: upstream.URL,
		Protocol: config.ProtocolGRPC,
	}
	front := httptest.NewServer(New(target, 1024, rec).Handler())
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	for _, want := range []string{"configured as grpc", "protocol: http"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the error should mention %q, got: %s", want, body)
		}
	}
	if call := rec.only(t); call.Error == "" {
		t.Error("the transport error should still be recorded")
	}
}

type plainOK struct{}

func (plainOK) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, "ok")
}
