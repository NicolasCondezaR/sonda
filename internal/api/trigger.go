package api

import (
	"encoding/json"
	"net/http"

	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/trigger"
)

// OnStored is the one hook the recorder calls for every capture it writes.
//
// The live view and the trigger both belong here rather than in main: they are
// two things done with the same call at the same moment, and the recorder only
// offers one hook. Both obey the same rule — neither may block, because this
// runs on the goroutine that persists.
func (s *Server) OnStored(c *store.Call) {
	s.hub.Publish(c)

	if s.trigger == nil {
		return
	}
	if fired := s.trigger.Observe(c.Summary()); fired != nil {
		// Announced to every live surface at once. What each of them does about
		// it — freeze, select, draw a banner — is theirs to decide; a trigger
		// that took the view away from someone reading it would be worse than
		// no trigger.
		s.hub.PublishNamed("trigger", map[string]any{
			"fired": fired,
			"state": s.trigger.State(),
		})
	}
}

func (s *Server) triggerState(w http.ResponseWriter, _ *http.Request) {
	if s.trigger == nil {
		writeJSON(w, http.StatusOK, trigger.State{})
		return
	}
	writeJSON(w, http.StatusOK, s.trigger.State())
}

// setTrigger arms a condition, or clears whatever is armed.
func (s *Server) setTrigger(w http.ResponseWriter, r *http.Request) {
	if s.trigger == nil {
		writeError(w, http.StatusServiceUnavailable, "this Sonda was built without a trigger")
		return
	}

	var body struct {
		Clear bool `json:"clear"`
		trigger.Condition
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be a trigger condition")
		return
	}

	if body.Clear {
		s.trigger.Clear()
		writeJSON(w, http.StatusOK, s.trigger.State())
		return
	}

	if err := s.trigger.Arm(body.Condition); err != nil {
		// The refusals are all "this would never fire" or "this would fire on
		// everything", so the message is the answer and not a code to look up.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.trigger.State())
}
