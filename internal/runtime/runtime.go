// Package runtime turns the stored configuration into running listeners.
//
// It is the only place that knows how a row in the database becomes an open
// port: the store keeps configuration and knows nothing about sockets, the
// supervisor opens sockets and knows nothing about projects, and this joins
// them. Reconcile is the single entry point, so every path that changes
// configuration — activating a project, editing a service, uploading a
// descriptor set — ends in the same call and cannot drift.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/protoschema"
	"github.com/NicolasCondezaR/sonda/internal/proxy"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/supervisor"
)

type Runtime struct {
	store      *store.Store
	recorder   proxy.Recorder
	supervisor *supervisor.Supervisor
	maxBody    int64

	// listenersDisabled keeps the runtime as a read-only view of the active
	// project, for callers that own their own sockets.
	listenersDisabled bool

	// stubs decides, per service, whether to answer from a recording instead
	// of forwarding. Nil means always forward.
	stubs proxy.Stubs

	// faults decides, per service, whether to break the call on purpose.
	faults proxy.Faults

	mu        sync.RWMutex
	active    *store.Project
	resolvers map[string]*protoschema.Resolver
	status    []supervisor.Status
}

// WithStubs lets the runtime answer for services from recordings. Separate
// from New because stubbing is an opt-in state, not part of being a runtime.
func (r *Runtime) WithStubs(s proxy.Stubs) *Runtime { r.stubs = s; return r }

// WithFaults lets the runtime break services on purpose.
func (r *Runtime) WithFaults(f proxy.Faults) *Runtime { r.faults = f; return r }

func New(db *store.Store, recorder proxy.Recorder, maxBody int64) *Runtime {
	return &Runtime{
		store:      db,
		recorder:   recorder,
		supervisor: supervisor.New(),
		maxBody:    maxBody,
		resolvers:  map[string]*protoschema.Resolver{},
	}
}

// NewWithoutListeners tracks the active project without opening any ports.
//
// It exists for the case where something else already owns the sockets — a test
// driving a proxy it started itself — and the API still needs to know which
// project is active and where its schemas come from. Reload is the counterpart
// of Reconcile for that mode.
func NewWithoutListeners(db *store.Store, recorder proxy.Recorder, maxBody int64) *Runtime {
	r := New(db, recorder, maxBody)
	r.listenersDisabled = true
	return r
}

// Reload refreshes the view of the active project without touching listeners.
func (r *Runtime) Reload(ctx context.Context) error {
	project, err := r.store.ActiveProject(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = project
	if project == nil {
		r.resolvers = map[string]*protoschema.Resolver{}
		return nil
	}
	r.resolvers = r.buildResolvers(project)
	return nil
}

// Reconcile brings the listeners in line with the stored configuration.
//
// Rebuilding the whole set on every change rather than diffing by hand is
// deliberate: the supervisor already leaves untouched listeners alone, and a
// hand-written diff here would be a second source of truth about what is
// running.
func (r *Runtime) Reconcile(ctx context.Context) error {
	if r.listenersDisabled {
		return r.Reload(ctx)
	}
	project, err := r.store.ActiveProject(ctx)
	if err != nil {
		return fmt.Errorf("read the active project: %w", err)
	}

	if project == nil {
		r.supervisor.StopAll()
		r.mu.Lock()
		r.active, r.resolvers, r.status = nil, map[string]*protoschema.Resolver{}, nil
		r.mu.Unlock()
		slog.Info("no active project, nothing is listening")
		return nil
	}

	resolvers := r.buildResolvers(project)

	desired := make([]supervisor.Desired, 0, len(project.Services))
	for _, svc := range project.Services {
		target := config.Target{
			Name:     svc.Name,
			Listen:   svc.Listen,
			Upstream: svc.Upstream,
			Protocol: svc.Protocol,
		}
		p := proxy.New(target, r.maxBody, taggedRecorder{
			inner:   r.recorder,
			project: project.Name,
		}, r.stubs, r.faults)
		// Keyed by service id, so renaming a service does not close its port
		// and moving it to another port does.
		want := supervisor.Desired{Key: fmt.Sprintf("svc-%d", svc.ID), Listen: svc.Listen}
		if svc.Protocol == config.ProtocolPostgres {
			// A database connection is framed messages from its first byte, so
			// there is no request an HTTP handler could be given.
			want.Serve = p.ServePostgres
		} else {
			want.Handler = p.Handler()
		}
		desired = append(desired, want)
	}

	status := r.supervisor.Apply(desired)

	r.mu.Lock()
	r.active, r.resolvers, r.status = project, resolvers, status
	r.mu.Unlock()

	slog.Info("project active", "project", project.Name, "services", len(project.Services))
	return nil
}

// buildResolvers gives every gRPC service in the project a schema resolver.
// The descriptor set belongs to the project, not to each service — one system
// compiles to one set of descriptors, and repeating the reference per service
// was the duplication that made a flat list tedious.
func (r *Runtime) buildResolvers(project *store.Project) map[string]*protoschema.Resolver {
	out := map[string]*protoschema.Resolver{}
	for _, svc := range project.Services {
		if svc.Protocol != config.ProtocolGRPC {
			continue
		}
		var reflectionAddr string
		if svc.Reflection {
			// Reflection goes straight to the service, not through the proxy:
			// Sonda asking itself would capture its own bookkeeping.
			reflectionAddr = strings.TrimPrefix(strings.TrimPrefix(svc.Upstream, "http://"), "https://")
		}
		out[svc.Name] = protoschema.NewResolverWithBytes(
			project.DescriptorSet, project.DescriptorName, reflectionAddr)
	}
	return out
}

// Active is the project whose ports are open, or nil.
func (r *Runtime) Active() *store.Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// ActiveName is the tag captures are written under.
func (r *Runtime) ActiveName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil {
		return ""
	}
	return r.active.Name
}

func (r *Runtime) Resolvers() map[string]*protoschema.Resolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolvers
}

// Status is what is really listening, which is not always what was configured.
func (r *Runtime) Status() []supervisor.Status {
	return r.supervisor.Status()
}

func (r *Runtime) Stop() { r.supervisor.StopAll() }

// taggedRecorder stamps every capture with the project it was taken under, so
// switching projects does not pour one system's traffic into another's field.
type taggedRecorder struct {
	inner   proxy.Recorder
	project string
}

func (t taggedRecorder) Record(call *store.Call) {
	call.Project = t.project
	t.inner.Record(call)
}
