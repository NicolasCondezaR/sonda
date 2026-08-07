package protoschema

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	demov1 "github.com/NicolasCondezaR/sonda/examples/grpcdemo/demo/v1"
	"github.com/NicolasCondezaR/sonda/examples/grpcdemo/ordersvc"
)

// descriptorSetPath is produced by `buf build -o examples/grpcdemo/descriptors.binpb`
// and committed, so the tests do not need a protobuf compiler installed.
func descriptorSetPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "examples", "grpcdemo", "descriptors.binpb")
}

func startService(t *testing.T, withReflection bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	ordersvc.Register(srv, withReflection)
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	return listener.Addr().String()
}

func sampleBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(ordersvc.SampleOrder("ORD-500"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestResolveFromDescriptorSet(t *testing.T) {
	r := NewResolver(descriptorSetPath(t), "")

	method, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	if method.Source != SourceDescriptorSet {
		t.Errorf("source = %q, want %q", method.Source, SourceDescriptorSet)
	}
	if got := string(method.Input.FullName()); got != "demo.v1.GetOrderRequest" {
		t.Errorf("input = %q", got)
	}
	if got := string(method.Output.FullName()); got != "demo.v1.Order" {
		t.Errorf("output = %q", got)
	}
}

func TestResolveFromReflection(t *testing.T) {
	r := NewResolver("", startService(t, true))

	method, err := r.Lookup(context.Background(), "demo.v1.Orders", "ListOrders")
	if err != nil {
		t.Fatal(err)
	}
	if method.Source != SourceReflection {
		t.Errorf("source = %q, want %q", method.Source, SourceReflection)
	}
	if got := string(method.Output.FullName()); got != "demo.v1.Order" {
		t.Errorf("output = %q", got)
	}
}

// Reflection is the preferred source but the optional one: a service that does
// not serve it must produce a clear reason, not a crash or a silent blank.
func TestReflectionOnAServiceWithoutItFailsClearly(t *testing.T) {
	r := NewResolver("", startService(t, false))

	_, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "reflection") {
		t.Errorf("the error should name reflection as the cause: %v", err)
	}

	source, statusErr := r.Status(context.Background())
	if source != SourceNone || statusErr == nil {
		t.Errorf("status = %q, %v; want an unresolved source with a reason", source, statusErr)
	}
}

// A descriptor set on disk covers the service that does not serve reflection,
// which is the whole reason both paths exist.
func TestDescriptorSetCoversAServiceWithoutReflection(t *testing.T) {
	r := NewResolver(descriptorSetPath(t), startService(t, false))

	method, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	if method.Source != SourceDescriptorSet {
		t.Errorf("source = %q, want the descriptor set", method.Source)
	}
}

func TestDecodeRendersFieldNames(t *testing.T) {
	r := NewResolver(descriptorSetPath(t), "")
	method, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder")
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(method.Output, sampleBytes(t))
	if err != nil {
		t.Fatal(err)
	}

	var order map[string]any
	if err := json.Unmarshal(decoded, &order); err != nil {
		t.Fatalf("the decoded output is not JSON: %v (%s)", err, decoded)
	}
	if order["id"] != "ORD-500" {
		t.Errorf("id = %v", order["id"])
	}
	if order["customer"] != "Comercial Andes SpA" {
		t.Errorf("customer = %v", order["customer"])
	}
	// Enums come out as their symbolic name, which is the point of having a
	// schema at all — the wire only carries the number 1.
	if order["status"] != "STATUS_PENDING" {
		t.Errorf("status = %v, want the enum name", order["status"])
	}
	lines, ok := order["lines"].([]any)
	if !ok || len(lines) != 2 {
		t.Fatalf("lines = %v", order["lines"])
	}
	// protojson renders int64 as a string, because JSON numbers cannot hold
	// the full range.
	total := order["total"].(map[string]any)
	if total["amountCents"] != "4320000" {
		t.Errorf("amountCents = %v", total["amountCents"])
	}
}

// A descriptor that does not match the bytes has to fail rather than invent a
// plausible message; the caller falls back to the schema-free view.
func TestDecodeRejectsBytesThatAreNotTheMessage(t *testing.T) {
	r := NewResolver(descriptorSetPath(t), "")
	method, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(method.Input, []byte{0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("expected a decoding error for bytes that are not this message")
	}
}

func TestLookupOfAnUnknownMethod(t *testing.T) {
	r := NewResolver(descriptorSetPath(t), "")
	if _, err := r.Lookup(context.Background(), "demo.v1.Orders", "NoSuchMethod"); err == nil {
		t.Error("expected an error for a method that is not in the schema")
	}
	if _, err := r.Lookup(context.Background(), "no.Such.Service", "Any"); err == nil {
		t.Error("expected an error for a service that is not in the schema")
	}
}

func TestBrokenDescriptorSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.binpb")
	if err := os.WriteFile(path, []byte("this is not a descriptor set"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(path, "")
	if _, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder"); err == nil {
		t.Error("expected an error for a file that is not a FileDescriptorSet")
	}
}

// Guard against the demo proto and the committed descriptor set drifting apart:
// a stale descriptors.binpb would make the schema tests pass against a schema
// nobody serves any more.
func TestCommittedDescriptorSetMatchesTheGeneratedCode(t *testing.T) {
	r := NewResolver(descriptorSetPath(t), "")
	method, err := r.Lookup(context.Background(), "demo.v1.Orders", "GetOrder")
	if err != nil {
		t.Fatal(err)
	}

	fromCode := (&demov1.Order{}).ProtoReflect().Descriptor()
	if method.Output.FullName() != fromCode.FullName() {
		t.Fatalf("descriptor set has %q, generated code has %q", method.Output.FullName(), fromCode.FullName())
	}
	if method.Output.Fields().Len() != fromCode.Fields().Len() {
		t.Errorf("Order has %d fields in the descriptor set and %d in the generated code — run `buf generate && buf build -o examples/grpcdemo/descriptors.binpb`",
			method.Output.Fields().Len(), fromCode.Fields().Len())
	}
}
