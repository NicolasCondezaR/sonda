package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/NicolasCondezaR/sonda/internal/discover"
	"github.com/NicolasCondezaR/sonda/internal/protoschema"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/supervisor"
)

// maxDescriptorSet bounds an upload. A real descriptor set for a large monorepo
// is under a megabyte; the Delpa one covering seventy-four services is 560 KB.
const maxDescriptorSet = 32 << 20

type projectJSON struct {
	ID       int64         `json:"id"`
	Name     string        `json:"name"`
	Active   bool          `json:"active"`
	Services []serviceJSON `json:"services"`

	HasDescriptor     bool   `json:"has_descriptor"`
	DescriptorName    string `json:"descriptor_name,omitempty"`
	DescriptorUpdated string `json:"descriptor_updated,omitempty"`
}

type serviceJSON struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Listen     string `json:"listen"`
	Upstream   string `json:"upstream"`
	Protocol   string `json:"protocol"`
	Reflection bool   `json:"reflection"`

	// TLS says Sonda answers this port with a certificate, and
	// InsecureSkipVerify that the upstream's certificate is not checked. Both
	// travel with the service and not just with the captures, because the
	// question "is what I am looking at verified" is asked of the configuration
	// as often as of a single call.
	TLS                bool `json:"tls"`
	InsecureSkipVerify bool `json:"insecure_skip_verify"`

	// Running is what is actually happening on the port, which is not always
	// what was configured — a port held by something else is the common case.
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`

	// PointAt is the line that makes the caller talk through Sonda. Handing
	// it over beats explaining it: the address is the one thing a person cannot
	// derive without reading the configuration back.
	PointAt string `json:"point_at"`
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.Projects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := statusByKey(s.rt.Status())

	out := make([]projectJSON, 0, len(projects))
	for _, p := range projects {
		out = append(out, toProjectJSON(p, status))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func statusByKey(all []supervisor.Status) map[string]supervisor.Status {
	byKey := make(map[string]supervisor.Status, len(all))
	for _, st := range all {
		byKey[st.Key] = st
	}
	return byKey
}

func toProjectJSON(p store.Project, status map[string]supervisor.Status) projectJSON {
	out := projectJSON{
		ID: p.ID, Name: p.Name, Active: p.Active,
		HasDescriptor:  p.DescriptorSet != nil,
		DescriptorName: p.DescriptorName,
		Services:       make([]serviceJSON, 0, len(p.Services)),
	}
	if !p.DescriptorUpdated.IsZero() {
		out.DescriptorUpdated = p.DescriptorUpdated.Format(timeLayout)
	}
	for _, svc := range p.Services {
		st := status[fmt.Sprintf("svc-%d", svc.ID)]
		out.Services = append(out.Services, serviceJSON{
			ID: svc.ID, Name: svc.Name, Listen: svc.Listen,
			Upstream: svc.Upstream, Protocol: svc.Protocol, Reflection: svc.Reflection,
			TLS: svc.TLS, InsecureSkipVerify: svc.InsecureSkipVerify,
			Running: st.Running, Error: st.Error,
			PointAt: pointAt(svc),
		})
	}
	return out
}

// pointAt writes the environment variable that redirects the caller.
//
// Sonda is an explicit proxy: it sees nothing until whoever makes the call is
// told to call it instead. That is the one step no amount of configuration
// screen removes, so the least it can do is hand over the exact line.
func pointAt(svc store.Service) string {
	name := strings.ToUpper(strings.ReplaceAll(svc.Name, "-", "_"))
	suffix := "_URL"
	if svc.Protocol == "grpc" {
		suffix = "_GRPC_URL"
	}
	// A TLS listener answers nothing on http://, so the line handed over has to
	// carry the scheme or it is an address that will not work.
	if svc.TLS {
		return name + suffix + "=https://" + svc.Listen
	}
	return name + suffix + "=" + svc.Listen
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected a JSON body with a name")
		return
	}
	project, err := s.store.CreateProject(r.Context(), body.Name)
	if err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toProjectJSON(*project, nil))
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected a JSON body with a name")
		return
	}
	if err := s.store.RenameProject(r.Context(), id, body.Name); err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.reconcile(w, r)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteProject(r.Context(), id); err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.reconcile(w, r)
}

func (s *Server) activateProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.ActivateProject(r.Context(), id); err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.reconcile(w, r)
}

func (s *Server) deactivateProjects(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeactivateProjects(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reconcile(w, r)
}

// uploadDescriptor stores the compiled schemas for a whole project.
//
// The bytes are parsed before they are stored: a file that is not a descriptor
// set would otherwise be accepted happily and show up as every message
// undecoded, with nothing pointing at the upload as the cause.
func (s *Server) uploadDescriptor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxDescriptorSet))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the upload")
		return
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "the upload is empty")
		return
	}

	services, err := protoschema.ParseDescriptorSet(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		name = "descriptors.binpb"
	}
	if err := s.store.SetDescriptorSet(r.Context(), id, name, raw); err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}

	if err := s.rt.Reconcile(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stored":   len(raw),
		"services": services,
		"name":     name,
	})
}

func (s *Server) saveService(w http.ResponseWriter, r *http.Request) {
	var body serviceJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected a JSON body describing the service")
		return
	}

	svc := store.Service{
		ID:         body.ID,
		Name:       strings.TrimSpace(body.Name),
		Listen:     strings.TrimSpace(body.Listen),
		Upstream:   strings.TrimSpace(body.Upstream),
		Protocol:   body.Protocol,
		Reflection: body.Reflection,

		TLS:                body.TLS,
		InsecureSkipVerify: body.InsecureSkipVerify,
	}

	if svc.ID == 0 {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		svc.ProjectID = id
	}

	if _, err := s.store.SaveService(r.Context(), svc); err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.reconcile(w, r)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteService(r.Context(), id); err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.reconcile(w, r)
}

// discoverServices reads a project's own configuration and reports what it
// found, without saving anything. Setting up fifteen services by hand is how a
// tool like this gets abandoned after one afternoon; the addresses are already
// written down, so this reads them instead of asking for them again.
func (s *Server) discoverServices(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the file")
		return
	}

	found, err := discover.Detect(filename, strings.NewReader(string(raw)))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Ports are probed here rather than at save time so a clash is visible in
	// the preview, beside the service it belongs to.
	type entry struct {
		discover.Found
		PortTaken bool   `json:"port_taken"`
		PortError string `json:"port_error,omitempty"`
	}
	out := make([]entry, 0, len(found))
	for _, f := range found {
		e := entry{Found: f}
		if err := supervisor.Probe(f.Listen); err != nil {
			e.PortTaken, e.PortError = true, err.Error()
		}
		out = append(out, e)
	}

	writeJSON(w, http.StatusOK, map[string]any{"found": out})
}

// runtimeStatus reports what is really listening.
func (s *Server) runtimeStatus(w http.ResponseWriter, _ *http.Request) {
	active := s.rt.Active()
	out := map[string]any{"listeners": s.rt.Status()}
	if active != nil {
		out["project"] = active.Name
		out["project_id"] = active.ID
	}
	writeJSON(w, http.StatusOK, out)
}

// tlsAuthority reports the certificate authority Sonda signs with, and what to
// run to trust it and to take it back out.
//
// The commands are the answer, not a link to one: the whole point of Sonda not
// touching the trust store is that the user performs the act, and telling them
// to "install the CA" without the exact line is how that becomes a shrug and a
// browser warning clicked through.
//
// The private key is not here, is not derivable from here, and there is no
// endpoint that returns it.
func (s *Server) tlsAuthority(w http.ResponseWriter, _ *http.Request) {
	ca := s.rt.CA()
	if ca == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"exists": false,
			"note": "Sonda has not created a certificate authority. It creates one the first time a service is set to terminate TLS, " +
				"and nothing is ever added to this machine's trust store on your behalf.",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":       true,
		"instructions": ca.Instructions(),
		"download":     "/api/tls/ca.pem",
	})
}

// tlsCertificate hands over the public certificate.
//
// The path in the instructions is enough when Sonda runs on the same machine as
// the client. It is not when Sonda runs in the container this repository ships,
// where the file exists somewhere the developer's browser cannot reach — so the
// bytes are downloadable. Only the certificate: the key has no endpoint.
func (s *Server) tlsCertificate(w http.ResponseWriter, _ *http.Request) {
	ca := s.rt.CA()
	if ca == nil {
		writeError(w, http.StatusNotFound, "there is no certificate authority yet; set a service to terminate TLS and Sonda creates one")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="sonda-ca.pem"`)
	_, _ = w.Write(ca.CertificatePEM())
}

// reconcile applies the stored configuration and answers with the new state.
// Every mutation ends here, so there is one path from "configuration changed"
// to "ports match it" and no chance of the two drifting.
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if err := s.rt.Reconcile(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.listProjects(w, r)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be a number")
		return 0, false
	}
	return id, true
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrProjectNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrDuplicateName):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}
