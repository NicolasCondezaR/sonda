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
	"github.com/NicolasCondezaR/sonda/internal/tlsca"
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

	// caDir is where the certificate authority is kept, and it is the database's
	// own directory: the CA key is as dangerous as the captures are, so the two
	// live together and are backed up, copied or deleted together.
	caDir string

	// applying serialises a whole reconcile: reading the configuration and
	// applying it are one decision, and mu cannot hold across both because the
	// readers of active and status must never wait on a listener opening.
	//
	// Without it two mutations interleave. Both read, both apply, and the one
	// holding the older view applies last — so a listener the other just
	// started is stopped again, and nothing schedules another reconcile to
	// notice. The port stays closed while every interface reports it open.
	// Several agents sharing one Sonda is a case the tools advertise, so this
	// is an ordinary sequence rather than a rare one.
	applying sync.Mutex

	mu        sync.RWMutex
	active    *store.Project
	resolvers map[string]*protoschema.Resolver
	status    []supervisor.Status

	// ca is created the first time a service actually asks Sonda to terminate
	// TLS, never at startup. A user who does not use the feature never finds a
	// certificate authority sitting in their project directory.
	ca *tlsca.CA
}

// WithStubs lets the runtime answer for services from recordings. Separate
// from New because stubbing is an opt-in state, not part of being a runtime.
func (r *Runtime) WithStubs(s proxy.Stubs) *Runtime { r.stubs = s; return r }

// WithFaults lets the runtime break services on purpose.
func (r *Runtime) WithFaults(f proxy.Faults) *Runtime { r.faults = f; return r }

// WithCADir says where the certificate authority lives. Callers pass the
// directory holding the database; the default is the working directory, which
// is where a database given as a bare filename ends up anyway.
func (r *Runtime) WithCADir(dir string) *Runtime {
	if dir != "" {
		r.caDir = dir
	}
	return r
}

// CA is the authority, or nil when nothing has needed one yet. The API reads it
// to tell the user how to trust it; there is deliberately no accessor anywhere
// for the private key.
func (r *Runtime) CA() *tlsca.CA {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ca
}

func New(db *store.Store, recorder proxy.Recorder, maxBody int64) *Runtime {
	return &Runtime{
		store:      db,
		recorder:   recorder,
		supervisor: supervisor.New(),
		maxBody:    maxBody,
		caDir:      ".",
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
	r.applying.Lock()
	defer r.applying.Unlock()
	return r.reload(ctx)
}

// reload is Reload with the reconcile lock already held.
func (r *Runtime) reload(ctx context.Context) error {
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
	// Held across the read and the apply both: see applying.
	r.applying.Lock()
	defer r.applying.Unlock()

	if r.listenersDisabled {
		return r.reload(ctx)
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

	// Asked for once per reconcile rather than per service: the authority is one
	// file on disk, and every TLS listener in the project answers from it.
	ca, err := r.certificateAuthority(project)
	if err != nil {
		return err
	}

	desired := make([]supervisor.Desired, 0, len(project.Services))
	for _, svc := range project.Services {
		target := config.Target{
			Name:               svc.Name,
			Listen:             svc.Listen,
			Upstream:           svc.Upstream,
			Protocol:           svc.Protocol,
			TLS:                svc.TLS,
			InsecureSkipVerify: svc.InsecureSkipVerify,
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
			if svc.TLS {
				want.TLS = ca.Config()
			}
		}
		desired = append(desired, want)
	}

	status := r.supervisor.Apply(desired)

	r.mu.Lock()
	r.active, r.resolvers, r.status, r.ca = project, resolvers, status, ca
	r.mu.Unlock()

	slog.Info("project active", "project", project.Name, "services", len(project.Services))
	return nil
}

// certificateAuthority returns the authority, opening it the first time a
// project actually wants a TLS listener.
//
// Once open it stays open even if every TLS service is later switched off: the
// files are still on disk and the user still needs to be told where they are and
// how to remove them.
func (r *Runtime) certificateAuthority(project *store.Project) (*tlsca.CA, error) {
	r.mu.RLock()
	existing := r.ca
	r.mu.RUnlock()
	if existing != nil {
		return existing, nil
	}

	wanted := false
	for _, svc := range project.Services {
		if svc.TLS {
			wanted = true
			break
		}
	}
	if !wanted {
		return nil, nil
	}

	ca, err := tlsca.Open(r.caDir)
	if err != nil {
		// Fatal for the whole reconcile rather than for one service: this is a
		// disk or permissions problem that affects every TLS listener equally,
		// and opening the ports as plaintext instead would be the tool
		// contradicting what every interface says about them.
		return nil, fmt.Errorf("open the certificate authority in %s: %w", r.caDir, err)
	}
	return ca, nil
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
