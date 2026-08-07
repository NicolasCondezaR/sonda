package api

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/NicolasCondezaR/sonda/internal/grpcwire"
	"github.com/NicolasCondezaR/sonda/internal/protoschema"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Resolvers maps a target name to the schema resolver for that service.
type Resolvers map[string]*protoschema.Resolver

type grpcView struct {
	Service string `json:"service"`
	Method  string `json:"method"`

	// Status is the real outcome. It is absent when the call never produced
	// one, which itself is worth seeing.
	Status     *int32 `json:"status,omitempty"`
	StatusText string `json:"status_text,omitempty"`
	Message    string `json:"message,omitempty"`

	Schema   schemaView    `json:"schema"`
	Request  []messageView `json:"request"`
	Response []messageView `json:"response"`

	// RequestIncomplete reports bytes left over after the last whole message:
	// the capture was cut off by the body cap or the stream was still running.
	RequestIncomplete  bool `json:"request_incomplete"`
	ResponseIncomplete bool `json:"response_incomplete"`
}

type schemaView struct {
	Source string `json:"source"`
	Error  string `json:"error,omitempty"`
}

type messageView struct {
	Index      int              `json:"index"`
	Size       int              `json:"size"`
	Compressed bool             `json:"compressed,omitempty"`
	JSON       json.RawMessage  `json:"json,omitempty"`
	Fields     []grpcwire.Field `json:"fields,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// buildGRPCView turns the stored bytes into something readable. Decoding
// happens here, at display time, and never on the way in — the capture stays
// exactly as it crossed the wire.
func (s *Server) buildGRPCView(ctx context.Context, c *store.Call) *grpcView {
	service, method, ok := grpcwire.MethodParts(c.Path)
	if !ok {
		return nil
	}

	view := &grpcView{
		Service: service,
		Method:  method,
		Status:  c.GRPCStatus,
		Message: c.GRPCMessage,
	}
	if c.GRPCStatus != nil {
		view.StatusText = codes.Code(*c.GRPCStatus).String()
	}

	var input, output protoreflect.MessageDescriptor
	if resolver := s.resolvers()[c.Target]; resolver != nil {
		found, err := resolver.Lookup(ctx, service, method)
		if err != nil {
			view.Schema.Error = err.Error()
		} else {
			input, output = found.Input, found.Output
			view.Schema.Source = string(found.Source)
		}
	} else {
		view.Schema.Error = "no schema source configured for this target"
	}

	view.Request, view.RequestIncomplete = decodeSide(c.Request.Body, input)
	view.Response, view.ResponseIncomplete = decodeSide(c.Response.Body, output)
	return view
}

func decodeSide(body []byte, desc protoreflect.MessageDescriptor) (views []messageView, incomplete bool) {
	frames, remainder := grpcwire.Deframe(body)
	views = make([]messageView, 0, len(frames))

	for i, frame := range frames {
		view := messageView{Index: i, Size: len(frame.Data), Compressed: frame.Compressed}

		switch {
		case frame.Compressed:
			// The encoding is negotiated per call and lives in a header;
			// guessing at it would produce confident garbage.
			view.Error = "compressed payload, not decoded"

		case desc != nil:
			decoded, err := protoschema.Decode(desc, frame.Data)
			if err != nil {
				// A schema that does not match the bytes is exactly when the
				// schema-free view earns its place.
				view.Error = "does not match the schema: " + err.Error()
				view.Fields, _ = grpcwire.Explain(frame.Data)
			} else {
				view.JSON = json.RawMessage(decoded)
			}

		default:
			fields, err := grpcwire.Explain(frame.Data)
			view.Fields = fields
			if err != nil {
				view.Error = err.Error()
			}
		}
		views = append(views, view)
	}
	return views, remainder > 0
}
