package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
	if err := checkTypes(tool.Schema, a); err != nil {
		// Reported as a tool error rather than a protocol one, for the same
		// reason every other bad call is: the model reads it and corrects the
		// argument instead of the client deciding the session is broken.
		return result(req.ID, toolResult(err.Error(), true))
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

// checkTypes holds the arguments to the types their schema declares.
//
// Coercing instead is how set_stub {"enabled":"1"} used to disable stubbing and
// report success: the argument was present, so the tool acted on it, and the
// value resolved to false somewhere on the way through. Every flag in this
// server has that shape — tls, detail, cut, clear, probe_upstreams — so the
// check belongs here, once, rather than in each of them.
//
// A property the schema does not describe is left alone: the schema is the
// contract, and this enforces the part of it that was written down.
func checkTypes(schema map[string]any, a args) error {
	properties, _ := schema["properties"].(map[string]any)
	for key, value := range a {
		declared, _ := properties[key].(map[string]any)
		kind, _ := declared["type"].(string)

		ok := true
		switch kind {
		case "boolean":
			_, ok = value.(bool)
		case "integer", "number":
			ok = isNumber(value)
		case "string":
			_, ok = value.(string)
		case "array":
			_, ok = value.([]any)
		}
		if !ok {
			return fmt.Errorf("%s must be %s, and %s arrived instead — Sonda will not guess what was meant, because guessing wrong here reports success for the opposite of what was asked",
				key, wanted(kind), describe(value))
		}
	}
	return nil
}

// isNumber accepts a number written as a string, which is the one coercion
// worth keeping: clients that assemble arguments out of text send "20", and it
// cannot mean anything other than 20.
func isNumber(v any) bool {
	switch n := v.(type) {
	case float64:
		return true
	case string:
		_, err := strconv.Atoi(strings.TrimSpace(n))
		return err == nil
	}
	return false
}

func wanted(kind string) string {
	switch kind {
	case "boolean":
		return "true or false"
	case "integer", "number":
		return "a number"
	case "array":
		return "a list"
	default:
		return "a " + kind
	}
}

func describe(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "true or false"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "a list"
	default:
		return "an object"
	}
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
