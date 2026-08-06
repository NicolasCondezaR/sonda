// Command mirador captures the traffic of the active project and serves the
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
	"sync"
	"syscall"
	"time"

	"mirador/internal/api"
	"mirador/internal/config"
	"mirador/internal/runtime"
	"mirador/internal/store"
	"mirador/internal/web"
)

// version reports the revision Go already embeds at build time, so a binary
// copied onto another machine can still say which commit it is. No ldflags to
// remember and no generated file to keep in sync.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "mirador (unknown build)"
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
		return "mirador (built without VCS information) " + info.GoVersion
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return "mirador " + revision + modified + " " + info.GoVersion
}

func main() {
	if err := run(); err != nil {
		slog.Error("mirador stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "mirador.yaml", "configuration file; its targets seed a fresh database")
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

	slog.Info("mirador ready", "ui", "http://"+cfg.APIListen, "database", cfg.Database)

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
