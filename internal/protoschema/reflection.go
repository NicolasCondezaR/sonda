package protoschema

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// The reflection service exists at two package names. Modern grpc-go serves
// both, but older servers and other language runtimes often only answer the
// v1alpha one, so both are tried.
//
// The two versions are wire-compatible — identical field numbers, only the
// package name differs — so one set of generated types drives both by calling
// the stream method path directly instead of going through two sets of stubs.
var reflectionMethods = []string{
	"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
	"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
}

const reflectionTimeout = 10 * time.Second

// fetchViaReflection asks a service to describe itself and builds a registry
// from the answer.
func fetchViaReflection(ctx context.Context, addr string) (*protoregistry.Files, error) {
	ctx, cancel := context.WithTimeout(ctx, reflectionTimeout)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()

	var lastErr error
	for _, method := range reflectionMethods {
		files, err := fetchWithMethod(ctx, conn, method)
		if err == nil {
			return files, nil
		}
		// Only an unimplemented service justifies trying the other version;
		// anything else is a real failure and retrying hides it.
		if status.Code(err) != codes.Unimplemented {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("the service does not serve reflection: %w", lastErr)
}

func fetchWithMethod(ctx context.Context, conn *grpc.ClientConn, method string) (*protoregistry.Files, error) {
	desc := &grpc.StreamDesc{StreamName: "ServerReflectionInfo", ServerStreams: true, ClientStreams: true}
	stream, err := conn.NewStream(ctx, desc, method)
	if err != nil {
		return nil, err
	}

	services, err := listServices(stream)
	if err != nil {
		return nil, err
	}

	// Descriptors arrive as whole files including transitive dependencies, and
	// the same file comes back for every symbol it defines, so they are
	// deduplicated by name before the registry is built.
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	for _, service := range services {
		if err := collectSymbol(stream, service, byName); err != nil {
			return nil, err
		}
	}
	_ = stream.CloseSend()

	if len(byName) == 0 {
		return nil, errors.New("reflection returned no descriptors")
	}

	set := &descriptorpb.FileDescriptorSet{File: make([]*descriptorpb.FileDescriptorProto, 0, len(byName))}
	for _, file := range byName {
		set.File = append(set.File, file)
	}
	// protodesc resolves dependencies by name, so the order files are added in
	// does not matter here.
	return protodesc.NewFiles(set)
}

func listServices(stream grpc.ClientStream) ([]string, error) {
	req := &reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}
	if err := stream.SendMsg(req); err != nil {
		return nil, err
	}
	var resp reflectionpb.ServerReflectionResponse
	if err := stream.RecvMsg(&resp); err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, fmt.Errorf("list services: %s", e.GetErrorMessage())
	}

	var services []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		name := s.GetName()
		// The reflection service describing itself is noise.
		if name == "grpc.reflection.v1.ServerReflection" || name == "grpc.reflection.v1alpha.ServerReflection" {
			continue
		}
		services = append(services, name)
	}
	if len(services) == 0 {
		return nil, errors.New("the service reports no services other than reflection itself")
	}
	return services, nil
}

func collectSymbol(stream grpc.ClientStream, symbol string, into map[string]*descriptorpb.FileDescriptorProto) error {
	req := &reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol},
	}
	if err := stream.SendMsg(req); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("reflection stream closed while asking for %s", symbol)
		}
		return err
	}

	var resp reflectionpb.ServerReflectionResponse
	if err := stream.RecvMsg(&resp); err != nil {
		return err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return fmt.Errorf("describe %s: %s", symbol, e.GetErrorMessage())
	}

	for _, raw := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
		file := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(raw, file); err != nil {
			return fmt.Errorf("descriptor for %s: %w", symbol, err)
		}
		into[file.GetName()] = file
	}
	return nil
}
