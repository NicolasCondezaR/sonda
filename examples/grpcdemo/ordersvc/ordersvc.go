// Package ordersvc implements the toy Orders service.
//
// It lives in its own package rather than in main so the tests exercise the
// same server the demo runs, instead of a lookalike written twice.
package ordersvc

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	demov1 "sonda/examples/grpcdemo/demo/v1"
)

type Server struct {
	demov1.UnimplementedOrdersServer
}

// Register wires the service into a gRPC server, optionally with reflection.
func Register(srv *grpc.Server, withReflection bool) {
	demov1.RegisterOrdersServer(srv, &Server{})
	if withReflection {
		reflection.Register(srv)
	}
}

func (s *Server) GetOrder(_ context.Context, req *demov1.GetOrderRequest) (*demov1.Order, error) {
	return SampleOrder(req.GetOrderId()), nil
}

func (s *Server) ListOrders(req *demov1.ListOrdersRequest, stream grpc.ServerStreamingServer[demov1.Order]) error {
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 20 {
		limit = 3
	}
	delay := time.Duration(req.GetDelayMs()) * time.Millisecond
	for i := range limit {
		if i > 0 && delay > 0 {
			select {
			case <-time.After(delay):
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
		}
		order := SampleOrder(fmt.Sprintf("ORD-%03d", i+1))
		if c := req.GetCustomer(); c != "" {
			order.Customer = c
		}
		if err := stream.Send(order); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) CreateOrders(stream grpc.ClientStreamingServer[demov1.Order, demov1.CreateOrdersSummary]) error {
	var created int32
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&demov1.CreateOrdersSummary{Created: created})
		}
		if err != nil {
			return err
		}
		created++
	}
}

// Fail answers with an error and no message, which produces a trailers-only
// response: the status arrives in the headers and there are no trailers at all.
func (s *Server) Fail(_ context.Context, req *demov1.FailRequest) (*demov1.Order, error) {
	code := codes.Code(req.GetCode())
	if code == codes.OK {
		code = codes.NotFound
	}
	message := req.GetMessage()
	if message == "" {
		message = "the demo service failed on purpose"
	}
	return nil, status.Error(code, message)
}

func SampleOrder(id string) *demov1.Order {
	if id == "" {
		id = "ORD-001"
	}
	return &demov1.Order{
		Id:       id,
		Customer: "Comercial Andes SpA",
		Lines: []*demov1.Line{
			{Sku: "ABC-9", Quantity: 3, Price: &demov1.Money{Currency: "CLP", AmountCents: 1290000}},
			{Sku: "XYZ-1", Quantity: 1, Price: &demov1.Money{Currency: "CLP", AmountCents: 450000}},
		},
		Total:  &demov1.Money{Currency: "CLP", AmountCents: 4320000},
		Status: demov1.Status_STATUS_PENDING,
	}
}
