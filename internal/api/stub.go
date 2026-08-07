package api

import (
	"encoding/json"
	"net/http"
)

// Stubbing is state a person can forget they turned on, so the endpoint that
// reports it matters as much as the one that sets it. Both answer with the
// complete picture rather than an acknowledgement.

func (s *Server) stubState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.stubSnapshot())
}

func (s *Server) setStub(w http.ResponseWriter, r *http.Request) {
	if s.stubs == nil {
		writeError(w, http.StatusServiceUnavailable, "this Sonda was built without stubbing")
		return
	}

	var body struct {
		Service string `json:"service"`
		Enabled *bool  `json:"enabled"`
		// Clear turns everything back to live traffic at once: the one button
		// that undoes a state you may not remember setting.
		Clear bool `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected a JSON body with a service and enabled")
		return
	}

	switch {
	case body.Clear:
		s.stubs.Clear()
	case body.Service == "" || body.Enabled == nil:
		writeError(w, http.StatusBadRequest, "pass a service and enabled, or clear")
		return
	default:
		// Refusing an unknown name keeps a typo from looking like it worked
		// and then silently forwarding to the real service all afternoon.
		if !s.isKnownService(body.Service) {
			writeError(w, http.StatusNotFound, "the active project has no service called "+body.Service)
			return
		}
		s.stubs.Set(body.Service, *body.Enabled)
	}

	writeJSON(w, http.StatusOK, s.stubSnapshot())
}

func (s *Server) stubSnapshot() map[string]any {
	active := s.stubs.Active()
	if active == nil {
		active = []string{}
	}
	return map[string]any{
		"stubbed": active,
		"note":    "Stubbed services answer from recordings and never reach the real service. Every such answer carries an X-Sonda-Stub header, and stubbing is forgotten when Sonda restarts.",
	}
}

func (s *Server) isKnownService(name string) bool {
	for _, t := range s.targets() {
		if t.Name == name {
			return true
		}
	}
	return false
}
