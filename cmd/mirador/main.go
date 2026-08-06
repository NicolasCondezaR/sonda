// Command mirador runs one capturing proxy per configured target plus the
// query API that serves what they captured.
package main

import (
	"context"
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
	"mirador/internal/protoschema"
	"mirador/internal/proxy"
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
	configPath := flag.String("config", "mirador.yaml", "path to the configuration file")
	showVersion := flag.Bool("version", false, "print the build this binary came from and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version())
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	recorder := store.NewRecorder(db, cfg.BufferSize)
	resolvers := buildResolvers(cfg)

	// Wire the live view before anything starts reading from the recorder.
	// Registering the hook once Run is already going is a data race on the
	// field it writes, and the one place it would surface is under load.
	apiServer := api.New(db, recorder, cfg.Targets, resolvers)
	recorder.OnStored(apiServer.Hub().Publish)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	fail := make(chan error, len(cfg.Targets)+1)

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

	for _, target := range cfg.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := proxy.New(target, cfg.MaxBodyBytes, recorder).Serve(ctx); err != nil {
				fail <- err
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := serveAPI(ctx, cfg, apiServer); err != nil {
			fail <- err
		}
	}()

	slog.Info("mirador ready", "ui", "http://"+cfg.APIListen, "api", cfg.APIListen, "targets", len(cfg.Targets), "database", cfg.Database)

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

// buildResolvers creates one schema resolver per gRPC target. Resolution is
// lazy: a service that is down when Mirador starts is the normal case, not an
// error worth refusing to boot over.
func buildResolvers(cfg *config.Config) api.Resolvers {
	resolvers := api.Resolvers{}
	for _, t := range cfg.Targets {
		if t.Protocol != config.ProtocolGRPC {
			continue
		}
		var reflectionAddr string
		if t.ReflectionEnabled() {
			// Reflection goes straight to the service, not through the proxy:
			// Mirador asking itself would capture its own bookkeeping.
			reflectionAddr = t.UpstreamURL().Host
		}
		resolvers[t.Name] = protoschema.NewResolver(t.DescriptorSet, reflectionAddr)
	}
	return resolvers
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

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
