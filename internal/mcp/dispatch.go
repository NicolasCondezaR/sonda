package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func (s *Server) listTools() map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		entry := map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": t.Schema,
		}
		if len(t.Annotations) > 0 {
			entry["annotations"] = t.Annotations
		}
		out = append(out, entry)
	}
	return map[string]any{"tools": out}
}

func (s *Server) callTool(ctx context.Context, req request) *response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return failure(req.ID, codeInvalidParams, "could not read the call parameters: %v", err)
	}

	var tool *Tool
	for i := range s.tools {
		if s.tools[i].Name == params.Name {
			tool = &s.tools[i]
			break
		}
	}
	if tool == nil {
		return failure(req.ID, codeInvalidParams, "there is no tool called %q", params.Name)
	}

	a := args{}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &a); err != nil {
			return failure(req.ID, codeInvalidParams, "arguments must be an object: %v", err)
		}
	}

	out, err := tool.Run(ctx, s, a)
	if err != nil {
		// A tool that fails is not a protocol error. The specification is
		// explicit that this comes back as a normal result with isError set,
		// so the model can read what went wrong and try something else
		// instead of the client treating the session as broken.
		return result(req.ID, toolResult(err.Error(), true))
	}

	text, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return failure(req.ID, codeInternal, "could not encode the answer: %v", err)
	}
	return result(req.ID, toolResult(string(text), false))
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// raw performs an API call and returns the payload, turning anything that is
// not a success into an error a model can act on.
func (s *Server) raw(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	status, payload, err := s.api.Call(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("Sonda answered %d: %s", status, apiError(payload))
	}
	return payload, nil
}

func (s *Server) get(ctx context.Context, path string, detail bool) (any, error) {
	payload, err := s.raw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return cleanJSON(payload, detail)
}

func (s *Server) post(ctx context.Context, path string, body []byte) (any, error) {
	payload, err := s.raw(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	return cleanJSON(payload, false)
}

// apiError pulls the message out of Sonda's error shape so the model reads
// "no call with that id" instead of a JSON envelope.
func apiError(payload []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(payload, &body) == nil && body.Error != "" {
		return body.Error
	}
	if len(payload) > 300 {
		payload = payload[:300]
	}
	return string(payload)
}
