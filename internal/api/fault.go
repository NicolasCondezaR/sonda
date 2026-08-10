package api

import (
	"encoding/json"
	"net/http"

	"github.com/NicolasCondezaR/sonda/internal/fault"
)

// A rule in force is state a person forgets they set, so the endpoint that
// reports it matters as much as the one that sets it. Both answer with the
// whole picture rather than an acknowledgement.

func (s *Server) faultState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.faultSnapshot())
}

func (s *Server) setFault(w http.ResponseWriter, r *http.Request) {
	if s.faults == nil {
		writeError(w, http.StatusServiceUnavailable, "this Sonda was built without fault injection")
		return
	}

	var body struct {
		Service string `json:"service"`
		fault.Rule
		// Clear removes the rule for one service; ClearAll removes every rule.
		Clear    bool `json:"clear"`
		ClearAll bool `json:"clear_all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected a JSON body with a service and a rule")
		return
	}

	switch {
	case body.ClearAll:
		s.faults.ClearAll()
	case body.Service == "":
		writeError(w, http.StatusBadRequest, "which service? pass its name, or clear_all")
		return
	case body.Clear:
		s.faults.Clear(body.Service)
	default:
		// An unknown name would look like it worked and then break nothing all
		// afternoon.
		if !s.isKnownService(body.Service) {
			writeError(w, http.StatusNotFound, "the active project has no service called "+body.Service)
			return
		}
		if err := s.faults.Set(body.Service, body.Rule); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, s.faultSnapshot())
}

func (s *Server) faultSnapshot() map[string]any {
	rules := s.faults.Rules()
	if rules == nil {
		rules = map[string]string{}
	}
	return map[string]any{
		"faults": rules,
		"note":   "Injected failures are recorded as injected and carry an X-Sonda-Fault header, so they can never be mistaken for the service's own. Rules are forgotten when Sonda restarts.",
	}
}
