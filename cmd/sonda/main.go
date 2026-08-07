// Command sonda captures the traffic of the active project and serves the
// interface that reads it.
//
// Which services are observed is not decided here. Projects live in the
// database and are edited from the interface; the configuration file only
// carries process-level settings and, on a fresh database, seeds the first
// project.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/api"
	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/mcp"
	"github.com/NicolasCondezaR/sonda/internal/runtime"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/web"
)

// release is the tag this binary was cut from, set only by the release build
// with -ldflags="-X main.release=v0.1.0". A plain `go build` leaves it empty,
// which is the honest answer: a build off a working tree is not a release, and
// a binary that claims a version it was not cut from is worse than one that
// admits it does not know.
var release string

// version reports the tag when there is one and the revision Go already embeds
// at build time, so a binary copied onto another machine can still say what it
// is. The revision needs no ldflags and no generated file to keep in sync.
func version() string {
	info, _ := debug.ReadBuildInfo()
	return formatVersion(release, info)
}

// buildVersion is the bare version, for a machine reading a handshake: the tag
// when there is one, the commit when there is not. version() is a sentence for
// a person looking at a terminal, and "sonda sonda v0.2.3 … go1.26" is what
// comes out when the two get confused.
func buildVersion() string {
	if release != "" {
		return release
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
			return setting.Value[:12]
		}
	}
	return "unknown"
}

// formatVersion is separate from version() only so it can be handed a build
// info that never existed. There is no way to fake the real one, and the
// interesting cases — installed from the module proxy, built from a checkout,
// built inside the image — differ precisely in what it contains.
func formatVersion(release string, info *debug.BuildInfo) string {
	parts := []string{"sonda"}

	if info == nil {
		if release != "" {
			parts = append(parts, release)
		}
		return strings.Join(append(parts, "(unknown build)"), " ")
	}

	// Two sources know the tag and neither covers the other. The release build
	// passes it in through ldflags; `go install` never sees those, but Go
	// records the module version it fetched — which is how a binary nobody
	// built from a checkout still knows what it is. "(devel)" is what a local
	// build reports, and it is not a version.
	tag := release
	if tag == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		tag = info.Main.Version
	}
	if tag != "" {
		parts = append(parts, tag)
	}

	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				modified = " (uncommitted changes)"
			}
		}
	}
	// The container image is built from a context without .git — keeping the
	// repository out of it is what makes the build context kilobytes instead of
	// megabytes — so there is no revision to report there. Saying so beats
	// printing a Go version alone and looking truncated.
	if revision == "" {
		parts = append(parts, "(built without VCS information)")
	} else {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		parts = append(parts, revision+modified)
	}
	return strings.Join(append(parts, info.GoVersion), " ")
}

func main() {
	// `sonda mcp` is a different program wearing the same binary: it speaks
	// the Model Context Protocol on stdin and stdout and forwards to a Sonda
	// that is already running. Agents that only support the pipe transport
	// start it themselves; the ones that can take a URL use /mcp instead and
	// need none of this.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := runMCP(os.Args[2:]); err != nil {
			slog.Error("sonda mcp stopped", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("sonda stopped", "error", err)
		os.Exit(1)
	}
}

func runMCP(argv []string) error {
	fs := flag.NewFlagSet("sonda mcp", flag.ExitOnError)
	addr := fs.String("api", "http://127.0.0.1:9000", "address of the running Sonda whose captures to expose")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	// stdout carries protocol messages and nothing else. A stray log line
	// there corrupts the stream, and the client reports a parse error with no
	// hint about where it came from.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	remote, err := mcp.NewRemote(*addr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return mcp.New(remote, buildVersion()).ServeStdio(ctx, os.Stdin, os.Stdout)
}

func run() error {
	configPath := flag.String("config", "sonda.yaml", "configuration file; its targets seed a fresh database")
	showVersion := flag.Bool("version", false, "print the build this binary came from and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version())
		return nil
	}

	cfg, err := config.LoadOrDefaults(*configPath)
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := seedFromConfig(ctx, db, cfg, *configPath); err != nil {
		return err
	}

	recorder := store.NewRecorder(db, cfg.BufferSize)
	rt := runtime.New(db, recorder, cfg.MaxBodyBytes)
	apiServer := api.New(db, recorder, rt)

	// Wire the live view before anything starts reading from the recorder.
	// Registering the hook once Run is already going is a data race on the
	// field it writes, and the one place it would surface is under load.
	recorder.OnStored(apiServer.Hub().Publish)

	if err := rt.Reconcile(ctx); err != nil {
		return err
	}
	defer rt.Stop()

	var wg sync.WaitGroup
	fail := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		recorder.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		pruneLoop(ctx, db, cfg.Retention)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := serveAPI(ctx, cfg, apiServer); err != nil {
			fail <- err
		}
	}()

	slog.Info("sonda ready", "ui", "http://"+cfg.APIListen, "database", cfg.Database)

	select {
	case err := <-fail:
		stop()
		wg.Wait()
		return err
	case <-ctx.Done():
		slog.Info("shutting down, draining captured calls")
		wg.Wait()
		return nil
	}
}

// seedFromConfig turns a configuration file into the first project, once.
//
// It runs only when the database holds no projects at all, so an edit made in
// the interface is never undone by a stale file on the next restart. Two
// sources of truth for the same thing is how a configuration screen stops
// being trusted.
func seedFromConfig(ctx context.Context, db *store.Store, cfg *config.Config, path string) error {
	if len(cfg.Targets) == 0 {
		return nil
	}
	projects, err := db.Projects(ctx)
	if err != nil {
		return err
	}
	if len(projects) > 0 {
		return nil
	}

	project, err := db.CreateProject(ctx, config.ProjectNameFor(path))
	if err != nil {
		return fmt.Errorf("seed the first project: %w", err)
	}

	for i, target := range cfg.Targets {
		if _, err := db.SaveService(ctx, store.Service{
			ProjectID:  project.ID,
			Name:       target.Name,
			Listen:     target.Listen,
			Upstream:   target.Upstream,
			Protocol:   target.Protocol,
			Reflection: target.ReflectionEnabled(),
			Position:   i,
		}); err != nil {
			return fmt.Errorf("seed service %s: %w", target.Name, err)
		}

		if target.DescriptorSet == "" {
			continue
		}
		blob, err := os.ReadFile(target.DescriptorSet)
		if err != nil {
			slog.Warn("could not read the descriptor set named in the configuration",
				"path", target.DescriptorSet, "error", err)
			continue
		}
		// The set belongs to the project: one system compiles to one set of
		// descriptors, and the file repeated the same path on every service.
		if err := db.SetDescriptorSet(ctx, project.ID, target.DescriptorSet, blob); err != nil {
			return err
		}
	}

	if err := db.ActivateProject(ctx, project.ID); err != nil {
		return err
	}
	slog.Info("seeded the first project from the configuration file",
		"project", project.Name, "services", len(cfg.Targets))
	return nil
}

func serveAPI(ctx context.Context, cfg *config.Config, apiServer *api.Server) error {
	mux := http.NewServeMux()
	mux.Handle("/api/", apiServer.Handler())
	mux.Handle("/health", apiServer.Handler())

	// One more endpoint and any agent can read the captures: no install, no
	// child process, and several agents pointed here see the same Sonda. The
	// tools run against the API handler directly, in this process.
	mux.Handle("/mcp", mcp.New(mcp.Local{Handler: apiServer.Handler()}, buildVersion()).Handler())

	mux.Handle("/", web.Handler())

	server := &http.Server{
		Addr:    cfg.APIListen,
		Handler: mux,
		// No write timeout: the live stream is a long-lived response and a
		// deadline would sever it on a quiet stack.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: %w", err)
	}
	return nil
}

func pruneLoop(ctx context.Context, db *store.Store, r config.Retention) {
	ticker := time.NewTicker(r.IntervalDuration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := db.Prune(context.WithoutCancel(ctx), r.MaxAgeDuration(), r.MaxCalls)
			if err != nil {
				slog.Error("retention", "error", err)
				continue
			}
			if deleted > 0 {
				slog.Info("retention removed calls", "count", deleted)
			}
		}
	}
}
