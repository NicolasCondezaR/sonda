// Command pricing is the bookshop's gRPC price service.
//
// It does not serve reflection, and that is the point of it: most services in
// the wild do not, so Sonda has to be given a compiled descriptor set before it
// can turn a protobuf field number into a field name. Nothing else in the test
// bed exercises that path.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	shopv1 "github.com/NicolasCondezaR/sonda/testbed/gen/shop/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The shop's price list. A map is the whole database this service needs.
var prices = map[string]int64{
	"DUNE":         1890,
	"PALE-FIRE":    1450,
	"SOLARIS":      1220,
	"RESTRICTED-1": 9900,
	"OUT-OF-PRINT": 7800,
}

type server struct {
	shopv1.UnimplementedPricingServer
}

func main() {
	addr := flag.String("addr", ":8103", "address to listen on")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}

	// No reflection.Register here on purpose. See the package comment.
	s := grpc.NewServer()
	shopv1.RegisterPricingServer(s, &server{})

	slog.Info("listening", "addr", *addr, "reflection", false)
	if err := s.Serve(lis); err != nil {
		slog.Error("serve", "error", err)
	}
}

// Quote answers a price, or fails with a gRPC status while the HTTP response
// is still 200 — the failure a tool that reads status codes calls a success.
func (s *server) Quote(_ context.Context, req *shopv1.QuoteRequest) (*shopv1.QuoteResponse, error) {
	if strings.HasPrefix(req.GetSku(), "RESTRICTED") {
		// A non-ASCII character on purpose: gRPC percent-encodes the message on
		// the wire, and Sonda decoding it back is visible only when there is
		// something to decode.
		return nil, status.Error(codes.PermissionDenied,
			"this title is held in the reserve collection — ask a librarian")
	}
	unit, ok := prices[req.GetSku()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no price for %q", req.GetSku())
	}
	return quote(req, unit), nil
}

// WatchQuote re-sends the quote a few times, so there is a capture with more
// than one message on it.
func (s *server) WatchQuote(req *shopv1.QuoteRequest, stream grpc.ServerStreamingServer[shopv1.QuoteResponse]) error {
	unit, ok := prices[req.GetSku()]
	if !ok {
		return status.Errorf(codes.NotFound, "no price for %q", req.GetSku())
	}
	for i := 0; i < 3; i++ {
		// The price drifts a cent per tick so the three messages are not
		// identical and a diff between them says something.
		if err := stream.Send(quote(req, unit+int64(i))); err != nil {
			return err
		}
		time.Sleep(80 * time.Millisecond)
	}
	return nil
}

func quote(req *shopv1.QuoteRequest, unit int64) *shopv1.QuoteResponse {
	qty := int64(req.GetQuantity())
	if qty <= 0 {
		qty = 1
	}
	currency := req.GetCurrency()
	if currency == "" {
		currency = "USD"
	}
	tier := shopv1.Tier_TIER_LIST
	if qty >= 5 {
		tier = shopv1.Tier_TIER_MEMBER
	}
	return &shopv1.QuoteResponse{
		Sku:        req.GetSku(),
		UnitCents:  unit,
		TotalCents: unit * qty,
		Currency:   currency,
		Tier:       tier,
	}
}
