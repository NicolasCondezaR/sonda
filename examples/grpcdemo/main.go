// Command grpcdemo is the toy gRPC upstream Sonda is developed and demoed
// against.
//
// The -reflection flag exists so both schema resolution paths can be exercised:
// with it on, Sonda discovers the schema by asking the server; with it off,
// it has to fall back to a descriptor set on disk, and then to the structural
// view of the wire format.
package main

import (
	"flag"
	"log/slog"
	"net"
	"os"

	"google.golang.org/grpc"

	"sonda/examples/grpcdemo/ordersvc"
)

func main() {
	addr := flag.String("addr", ":8082", "address to listen on")
	useReflection := flag.Bool("reflection", true, "serve gRPC server reflection")
	flag.Parse()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("grpcdemo could not listen", "addr", *addr, "error", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	ordersvc.Register(srv, *useReflection)

	slog.Info("grpcdemo listening", "addr", *addr, "reflection", *useReflection)
	if err := srv.Serve(listener); err != nil {
		slog.Error("grpcdemo stopped", "error", err)
		os.Exit(1)
	}
}
