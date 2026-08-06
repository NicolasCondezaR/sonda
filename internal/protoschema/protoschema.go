// Package protoschema resolves protobuf schemas so captured gRPC messages can
// be shown with field names instead of field numbers.
//
// Two sources, tried in order: a descriptor set compiled to disk, and the
// service's own reflection API. Neither is required — when both come up empty
// the caller falls back to the structural view in package grpcwire, so a
// missing schema degrades the display rather than breaking it.
package protoschema

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Source records where a schema came from, so the UI can say so. A decoded
// message is only as trustworthy as the descriptor behind it, and a stale
// descriptor set produces confident nonsense.
type Source string

const (
	SourceNone          Source = ""
	SourceDescriptorSet Source = "descriptor_set"
	SourceReflection    Source = "reflection"
)

// retryAfter bounds how often a failed resolution is retried. Services are
// routinely down when Mirador starts, so giving up permanently would be wrong;
// retrying on every request would hammer a service that is already struggling.
const retryAfter = 30 * time.Second

// Method is the pair of message shapes for one RPC.
type Method struct {
	Input  protoreflect.MessageDescriptor
	Output protoreflect.MessageDescriptor
	Source Source
}

// Resolver holds the schema for one target and resolves it lazily.
type Resolver struct {
	descriptorSetPath string
	reflectionAddr    string // empty when reflection is disabled

	mu        sync.Mutex
	files     *protoregistry.Files
	source    Source
	lastTry   time.Time
	lastError error
}

func NewResolver(descriptorSetPath, reflectionAddr string) *Resolver {
	return &Resolver{descriptorSetPath: descriptorSetPath, reflectionAddr: reflectionAddr}
}

// Lookup finds the input and output messages for a fully qualified method,
// for example "demo.v1.Orders" and "GetOrder".
func (r *Resolver) Lookup(ctx context.Context, service, method string) (Method, error) {
	files, source, err := r.resolve(ctx)
	if err != nil {
		return Method{}, err
	}

	desc, err := files.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return Method{}, fmt.Errorf("service %s not in the schema: %w", service, err)
	}
	serviceDesc, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return Method{}, fmt.Errorf("%s is not a service", service)
	}
	methodDesc := serviceDesc.Methods().ByName(protoreflect.Name(method))
	if methodDesc == nil {
		return Method{}, fmt.Errorf("method %s not found on service %s", method, service)
	}
	return Method{Input: methodDesc.Input(), Output: methodDesc.Output(), Source: source}, nil
}

// Status reports where the schema came from, resolving it if that has not been
// tried yet.
//
// It resolves rather than just reporting cached state because this is the
// diagnostics path: the question behind it is "why are there no field names?",
// and answering "nothing has been attempted yet" would be useless. The cooldown
// in resolve still applies, so asking repeatedly costs nothing.
func (r *Resolver) Status(ctx context.Context) (source Source, err error) {
	if _, source, err := r.resolve(ctx); err == nil {
		return source, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source, r.lastError
}

func (r *Resolver) resolve(ctx context.Context) (*protoregistry.Files, Source, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.files != nil {
		return r.files, r.source, nil
	}
	if !r.lastTry.IsZero() && time.Since(r.lastTry) < retryAfter {
		return nil, SourceNone, r.lastError
	}
	r.lastTry = time.Now()

	if r.descriptorSetPath != "" {
		files, err := loadDescriptorSet(r.descriptorSetPath)
		if err == nil {
			r.files, r.source, r.lastError = files, SourceDescriptorSet, nil
			return files, SourceDescriptorSet, nil
		}
		r.lastError = fmt.Errorf("descriptor set: %w", err)
	}

	if r.reflectionAddr != "" {
		files, err := fetchViaReflection(ctx, r.reflectionAddr)
		if err == nil {
			r.files, r.source, r.lastError = files, SourceReflection, nil
			return files, SourceReflection, nil
		}
		if r.lastError == nil {
			r.lastError = fmt.Errorf("reflection: %w", err)
		} else {
			r.lastError = fmt.Errorf("%w; reflection: %w", r.lastError, err)
		}
	}

	if r.lastError == nil {
		r.lastError = fmt.Errorf("no schema source configured")
	}
	return nil, SourceNone, r.lastError
}

func loadDescriptorSet(path string) (*protoregistry.Files, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("%s is not a FileDescriptorSet: %w", path, err)
	}
	return protodesc.NewFiles(&set)
}

// Decode renders a message with its schema. Field numbers become names and
// enums become their symbolic values.
func Decode(desc protoreflect.MessageDescriptor, data []byte) ([]byte, error) {
	message := dynamicpb.NewMessage(desc)
	// Unknown fields are kept rather than rejected: a descriptor one version
	// behind the service is the common case, and dropping the fields it does
	// not know about would hide exactly what changed.
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, message); err != nil {
		return nil, err
	}
	return protojson.MarshalOptions{
		Multiline:       true,
		Indent:          "  ",
		EmitUnpopulated: true,
	}.Marshal(message)
}
