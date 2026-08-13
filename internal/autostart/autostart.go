// Package autostart manages Sonda's opt-in, per-user Windows logon task.
//
// The package keeps Task Scheduler and process-control calls behind narrow
// interfaces. The lifecycle can therefore be tested without registering a
// real task or stopping a process on the developer's machine.
package autostart

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
)

var (
	ErrNotInstalled = errors.New("Sonda autostart is not installed")
	ErrNotRunning   = errors.New("Sonda autostart is not running")
	ErrUnsupported  = errors.New("Sonda autostart is only supported on Windows")
)

type scheduler interface {
	Query(context.Context, string) ([]byte, error)
	Register(context.Context, string, []byte) error
	Start(context.Context, string) error
	End(context.Context, string) error
	Delete(context.Context, string) error
}

type processControl interface {
	Probe(string) bool
	Signal(string) error
	Generation(string) (string, bool, error)
}

type runtimeState struct {
	healthy          bool
	controlEvent     bool
	generation       string
	generationActive bool
	generationErr    error
}

type Launcher struct {
	Path     string
	Portable bool
	Warning  string
}

type dependencies struct {
	scheduler         scheduler
	control           processControl
	currentSID        func() (string, error)
	resolveAccountSID func(string) (string, error)
	resolveLauncher   func() (Launcher, error)
	homeDir           func() (string, error)
	stat              func(string) (os.FileInfo, error)
	mkdirAll          func(string, os.FileMode) error
	httpClient        *http.Client
	waitAttempts      int
	pollInterval      time.Duration
}

// Service owns one task for the current user. Task names include a digest of
// the SID, so two users on one workstation never overwrite each other's task.
type Service struct {
	deps dependencies
}

type InstallOptions struct {
	ConfigPath       string
	AllowNonLoopback bool
}

// ManageOptions identifies the exact configuration bound to an installed
// task. The default is the same per-user path used by Install. Callers that
// installed a custom path must provide it again so task ownership is never
// inferred from the task's own, potentially untrusted metadata.
type ManageOptions struct {
	ConfigPath string
}

type Status struct {
	Installed        bool
	TaskName         string
	TaskValid        bool
	Problem          string
	Launcher         string
	PortableLauncher bool
	ConfigPath       string
	ConfigExists     bool
	WorkingDirectory string
	LogPath          string
	HealthURL        string
	Healthy          bool
	ManagedProcess   bool
	AbruptStop       bool
	Warning          string
}

func newService(deps dependencies) *Service {
	if deps.stat == nil {
		deps.stat = os.Stat
	}
	if deps.mkdirAll == nil {
		deps.mkdirAll = os.MkdirAll
	}
	if deps.httpClient == nil {
		deps.httpClient = &http.Client{Timeout: time.Second}
	}
	if deps.waitAttempts <= 0 {
		deps.waitAttempts = 40
	}
	if deps.pollInterval <= 0 {
		deps.pollInterval = 250 * time.Millisecond
	}
	return &Service{deps: deps}
}

func (s *Service) Install(ctx context.Context, options InstallOptions) (Status, error) {
	spec, cfg, launcher, err := s.canonicalTask(options.ConfigPath, true)
	if err != nil {
		return Status{}, err
	}
	if !options.AllowNonLoopback && !loopbackListen(cfg.APIListen) {
		return Status{}, fmt.Errorf("api_listen %q is not loopback; pass -allow-non-loopback only after accepting that the unauthenticated API will be reachable beyond this machine", cfg.APIListen)
	}
	if _, err := s.deps.stat(spec.Launcher); err != nil {
		return Status{}, fmt.Errorf("launcher %s is not usable: %w", spec.Launcher, err)
	}

	existing, queryErr := s.deps.scheduler.Query(ctx, spec.Name)
	switch {
	case queryErr == nil:
		doc, metadata, parseErr := parseTask(existing)
		if parseErr == nil {
			status := s.statusFromTask(ctx, spec, doc, metadata)
			if !status.TaskValid {
				return status, fmt.Errorf("refusing to replace occupied task %s: %s", spec.Name, status.Problem)
			}
			status.Warning = launcher.Warning
			if status.Healthy && status.ManagedProcess {
				return status, nil
			}
			if status.Healthy && !status.ManagedProcess {
				return status, fmt.Errorf("%s is already served by a process not linked to task %s", status.HealthURL, status.TaskName)
			}
			return s.startSpec(ctx, spec, cfg, status)
		}
		status := s.unverifiedStatus(ctx, spec, parseErr)
		return status, fmt.Errorf("refusing to replace occupied task %s: %s", spec.Name, status.Problem)
	case errors.Is(queryErr, ErrNotInstalled):
		if s.healthy(ctx, healthURL(cfg.APIListen)) {
			return Status{TaskName: spec.Name, Healthy: true, HealthURL: healthURL(cfg.APIListen)},
				fmt.Errorf("refusing to install while %s is already served by another process", healthURL(cfg.APIListen))
		}
	default:
		return Status{TaskName: spec.Name}, fmt.Errorf("query scheduled task: %w", queryErr)
	}

	raw, err := marshalTask(spec)
	if err != nil {
		return Status{TaskName: spec.Name}, err
	}
	if err := s.deps.scheduler.Register(ctx, spec.Name, raw); err != nil {
		return Status{TaskName: spec.Name}, fmt.Errorf("register scheduled task without elevation: %w", err)
	}

	status := Status{
		Installed: true, TaskName: spec.Name, TaskValid: true,
		Launcher: spec.Launcher, PortableLauncher: spec.Metadata.PortableLauncher,
		ConfigPath: spec.Metadata.ConfigPath, ConfigExists: fileExists(s.deps.stat, spec.Metadata.ConfigPath),
		WorkingDirectory: spec.WorkingDirectory, LogPath: spec.Metadata.LogPath,
		HealthURL: spec.Metadata.HealthURL, Warning: launcher.Warning,
	}
	return s.startSpec(ctx, spec, cfg, status)
}

func (s *Service) Status(ctx context.Context, options ManageOptions) (Status, error) {
	status, _, err := s.inspectTask(ctx, options)
	if errors.Is(err, ErrNotInstalled) {
		return status, nil
	}
	return status, err
}

func (s *Service) Start(ctx context.Context, options ManageOptions) (Status, error) {
	status, spec, err := s.readManagedTask(ctx, options)
	if err != nil {
		return status, err
	}
	if status.Healthy && status.ManagedProcess {
		return status, nil
	}
	if status.Healthy {
		return status, fmt.Errorf("%s is served by a process not linked to task %s", status.HealthURL, status.TaskName)
	}
	verified, err := s.requireTask(ctx, spec)
	if err != nil {
		return verified, fmt.Errorf("refusing to start task %s after ownership changed: %w", spec.Name, err)
	}
	status = verified
	return s.runNewGeneration(ctx, spec, status, "task")
}

func (s *Service) Stop(ctx context.Context, options ManageOptions) (Status, error) {
	status, spec, err := s.readManagedTask(ctx, options)
	if err != nil {
		return status, err
	}
	return s.stopSpec(ctx, spec, status)
}

func (s *Service) Restart(ctx context.Context, options ManageOptions) (Status, error) {
	status, err := s.Stop(ctx, options)
	if err != nil {
		return status, err
	}
	return s.Start(ctx, options)
}

func (s *Service) Uninstall(ctx context.Context, options ManageOptions) (Status, error) {
	status, spec, err := s.readManagedTask(ctx, options)
	if errors.Is(err, ErrNotInstalled) {
		return status, nil
	}
	if err != nil {
		return status, err
	}

	stopped, stopErr := s.stopSpec(ctx, spec, status)
	verified, verifyErr := s.requireTask(ctx, spec)
	if errors.Is(verifyErr, ErrNotInstalled) {
		stopped.Installed = false
		stopped.TaskValid = false
		return stopped, stopErr
	}
	if verifyErr != nil {
		return verified, fmt.Errorf("refusing to delete task %s after ownership changed: %w", spec.Name, verifyErr)
	}
	stopped = verified
	if err := s.deps.scheduler.Delete(ctx, spec.Name); err != nil && !errors.Is(err, ErrNotInstalled) {
		return stopped, fmt.Errorf("delete scheduled task: %w", err)
	}
	stopped.Installed = false
	stopped.TaskValid = false
	if stopErr != nil {
		return stopped, fmt.Errorf("task removed after an incomplete stop: %w", stopErr)
	}
	return stopped, nil
}

func (s *Service) canonicalTask(configPathOption string, prepareState bool) (TaskSpec, *config.Config, Launcher, error) {
	if s.deps.scheduler == nil || s.deps.currentSID == nil || s.deps.resolveLauncher == nil || s.deps.homeDir == nil {
		return TaskSpec{}, nil, Launcher{}, ErrUnsupported
	}
	sid, err := s.deps.currentSID()
	if err != nil {
		return TaskSpec{}, nil, Launcher{}, fmt.Errorf("read current Windows SID: %w", err)
	}
	configPath, err := s.configPath(configPathOption)
	if err != nil {
		return TaskSpec{}, nil, Launcher{}, err
	}
	workingDirectory := filepath.Dir(configPath)
	if prepareState {
		if err := s.deps.mkdirAll(workingDirectory, 0o700); err != nil {
			return TaskSpec{}, nil, Launcher{}, fmt.Errorf("create Sonda state directory: %w", err)
		}
	}
	cfg, err := config.LoadOrDefaults(configPath)
	if err != nil {
		return TaskSpec{}, nil, Launcher{}, fmt.Errorf("validate autostart config: %w", err)
	}
	launcher, err := s.deps.resolveLauncher()
	if err != nil {
		return TaskSpec{}, nil, Launcher{}, err
	}
	launcher.Path, err = filepath.Abs(launcher.Path)
	if err != nil {
		return TaskSpec{}, nil, Launcher{}, fmt.Errorf("resolve launcher: %w", err)
	}

	taskName := taskNameForSID(sid)
	metadata := Metadata{
		Version:          1,
		ConfigPath:       configPath,
		LogPath:          filepath.Join(workingDirectory, "sonda.log"),
		ControlID:        controlID(taskName, configPath),
		HealthURL:        healthURL(cfg.APIListen),
		PortableLauncher: launcher.Portable,
	}
	spec := TaskSpec{
		Name:             taskName,
		SID:              sid,
		Launcher:         launcher.Path,
		WorkingDirectory: workingDirectory,
		Metadata:         metadata,
	}
	spec.Arguments = windowsCommandLine([]string{
		"-config", metadata.ConfigPath,
		"-log-file", metadata.LogPath,
		"-autostart-control", metadata.ControlID,
	})
	return spec, cfg, launcher, nil
}

func (s *Service) inspectTask(ctx context.Context, options ManageOptions) (Status, TaskSpec, error) {
	taskName, err := s.taskName()
	if err != nil {
		return Status{}, TaskSpec{}, err
	}
	raw, err := s.deps.scheduler.Query(ctx, taskName)
	if errors.Is(err, ErrNotInstalled) {
		return Status{TaskName: taskName}, TaskSpec{}, ErrNotInstalled
	}
	if err != nil {
		return Status{TaskName: taskName}, TaskSpec{}, fmt.Errorf("query scheduled task: %w", err)
	}

	spec, _, _, err := s.canonicalTask(options.ConfigPath, false)
	if err != nil {
		return Status{Installed: true, TaskName: taskName, Problem: fmt.Sprintf("cannot derive canonical task: %v", err)}, TaskSpec{}, nil
	}
	doc, metadata, err := parseTask(raw)
	if err != nil {
		return s.unverifiedStatus(ctx, spec, err), spec, nil
	}
	return s.statusFromTask(ctx, spec, doc, metadata), spec, nil
}

func (s *Service) readManagedTask(ctx context.Context, options ManageOptions) (Status, TaskSpec, error) {
	status, spec, err := s.inspectTask(ctx, options)
	if errors.Is(err, ErrNotInstalled) {
		return status, spec, ErrNotInstalled
	}
	if err != nil {
		return status, spec, err
	}
	if !status.TaskValid {
		return status, spec, fmt.Errorf("task %s is not the canonical Sonda task for this user and config: %s", status.TaskName, status.Problem)
	}
	return status, spec, nil
}

func (s *Service) startSpec(ctx context.Context, spec TaskSpec, _ *config.Config, status Status) (Status, error) {
	verified, err := s.requireTask(ctx, spec)
	if err != nil {
		return verified, fmt.Errorf("refusing to start task %s before ownership was verified: %w", spec.Name, err)
	}
	status = verified
	return s.runNewGeneration(ctx, spec, status, "registered task")
}

func (s *Service) stopSpec(ctx context.Context, spec TaskSpec, status Status) (Status, error) {
	if !status.TaskValid {
		return status, fmt.Errorf("refusing to stop task %s because its ownership is not canonical", spec.Name)
	}
	if !status.ManagedProcess {
		if status.Healthy {
			return status, fmt.Errorf("refusing to stop: %s is served by a process not linked to task %s", status.HealthURL, spec.Name)
		}
		state := s.runtimeState(ctx, spec.Metadata)
		if state.generationErr != nil {
			return status, fmt.Errorf("inspect managed process generation before stop: %w", state.generationErr)
		}
		if !state.generationActive {
			return status, nil
		}
		if final, stopped := s.waitForStopped(ctx, spec.Metadata); stopped {
			return s.statusForSpec(ctx, spec)
		} else {
			return status, fmt.Errorf("task %s control event and health endpoint are inactive, but its previous process generation did not exit within %s (%s)", spec.Name, s.waitLimit(), final.describe())
		}
	}
	if err := s.deps.control.Signal(spec.Metadata.ControlID); err == nil {
		if _, stopped := s.waitForStopped(ctx, spec.Metadata); stopped {
			return s.statusForSpec(ctx, spec)
		}
	}
	state := s.runtimeState(ctx, spec.Metadata)
	if !state.controlEvent {
		if state.generationErr != nil {
			return status, fmt.Errorf("managed control event closed and the process generation could not be inspected; refusing Task Scheduler fallback: %w", state.generationErr)
		}
		if state.generationActive {
			return status, fmt.Errorf("managed control event closed, but the same process generation did not exit before timeout; refusing Task Scheduler fallback")
		}
		return status, fmt.Errorf("managed process exited, but %s is now served by another process; refusing Task Scheduler fallback", spec.Metadata.HealthURL)
	}

	// The fallback is permitted only while the expected control event still
	// proves that the process belongs to the canonical task and a fresh query
	// confirms that the task did not change during graceful signaling.
	verified, err := s.requireTask(ctx, spec)
	if err != nil {
		return verified, fmt.Errorf("refusing Task Scheduler fallback for task %s after ownership changed: %w", spec.Name, err)
	}
	if !verified.ManagedProcess {
		return verified, fmt.Errorf("refusing Task Scheduler fallback for task %s because its managed control event no longer exists", spec.Name)
	}
	status = verified
	status.AbruptStop = true
	if err := s.deps.scheduler.End(ctx, spec.Name); err != nil && !errors.Is(err, ErrNotRunning) {
		return status, fmt.Errorf("graceful stop failed and Task Scheduler could not end task %s: %w", spec.Name, err)
	}
	if final, stopped := s.waitForStopped(ctx, spec.Metadata); !stopped {
		status, _ = s.statusForSpec(ctx, spec)
		status.AbruptStop = true
		return status, fmt.Errorf("task %s was ended by Task Scheduler but did not stop completely within %s (%s)", spec.Name, s.waitLimit(), final.describe())
	}
	status, _ = s.statusForSpec(ctx, spec)
	status.AbruptStop = true
	status.Warning = "the managed shutdown event remained active after graceful signaling failed; Task Scheduler ended the verified task abruptly"
	return status, nil
}

func (s *Service) statusFromTask(ctx context.Context, spec TaskSpec, doc taskDocument, metadata Metadata) Status {
	status := Status{
		Installed: true, TaskName: spec.Name,
		Launcher: doc.Actions.Exec.Command, PortableLauncher: metadata.PortableLauncher,
		ConfigPath: spec.Metadata.ConfigPath, ConfigExists: fileExists(s.deps.stat, spec.Metadata.ConfigPath),
		WorkingDirectory: doc.Actions.Exec.WorkingDirectory,
		LogPath:          spec.Metadata.LogPath, HealthURL: spec.Metadata.HealthURL,
		Healthy: s.healthy(ctx, spec.Metadata.HealthURL), ManagedProcess: s.deps.control.Probe(spec.Metadata.ControlID),
	}
	status.TaskValid, status.Problem = s.validateTask(spec, doc, metadata)
	if status.PortableLauncher {
		status.Warning = "the task is bound to a portable/local executable; install only after placing it at its final path, and restore that path or remove the task manually before reinstalling after a move"
	}
	if status.Healthy && !status.ManagedProcess {
		status.Warning = strings.TrimSpace(status.Warning + "; the health address is active, but the managed control event is absent")
	}
	return status
}

func (s *Service) validateTask(spec TaskSpec, doc taskDocument, metadata Metadata) (bool, string) {
	if mismatch := s.taskMismatchCurrentUser(spec, doc, metadata); mismatch != "" {
		return false, "canonical field mismatch: " + mismatch
	}
	if !filepath.IsAbs(spec.Launcher) || !filepath.IsAbs(spec.Metadata.ConfigPath) || !filepath.IsAbs(spec.WorkingDirectory) {
		return false, "launcher, config, or working directory is not absolute"
	}
	if _, err := s.deps.stat(spec.Launcher); err != nil {
		return false, fmt.Sprintf("launcher is missing: %v", err)
	}
	if _, err := s.deps.stat(spec.WorkingDirectory); err != nil {
		return false, fmt.Sprintf("working directory is missing: %v", err)
	}
	return true, ""
}

func (s *Service) taskMismatchCurrentUser(spec TaskSpec, doc taskDocument, metadata Metadata) string {
	if !s.sameUserSID(doc.Principals.Principal.UserID, spec.SID) {
		return "principal.user_id"
	}
	if !s.sameUserSID(doc.Triggers.Logon.UserID, spec.SID) {
		return "trigger.user_id"
	}
	if doc.RegistrationInfo.Author != "Sonda" && !s.sameUserSID(doc.RegistrationInfo.Author, spec.SID) {
		return "registration.author"
	}

	// Task Scheduler may serialize these fields as MACHINE\User even when the
	// registered XML used a SID or a product author. Normalize only after Windows
	// resolves the representation back to the SID obtained from the current
	// token. Author is descriptive; principal and trigger remain authoritative.
	doc.Principals.Principal.UserID = spec.SID
	doc.Triggers.Logon.UserID = spec.SID
	doc.RegistrationInfo.Author = "Sonda"
	return taskMismatch(spec, doc, metadata)
}

func (s *Service) sameUserSID(identifier, currentSID string) bool {
	if strings.EqualFold(identifier, currentSID) {
		return true
	}
	if s.deps.resolveAccountSID == nil {
		return false
	}
	resolved, err := s.deps.resolveAccountSID(identifier)
	return err == nil && strings.EqualFold(resolved, currentSID)
}

func (s *Service) unverifiedStatus(ctx context.Context, spec TaskSpec, cause error) Status {
	return Status{
		Installed: true, TaskName: spec.Name,
		ConfigPath: spec.Metadata.ConfigPath, ConfigExists: fileExists(s.deps.stat, spec.Metadata.ConfigPath),
		WorkingDirectory: spec.WorkingDirectory, LogPath: spec.Metadata.LogPath,
		HealthURL: spec.Metadata.HealthURL, Healthy: s.healthy(ctx, spec.Metadata.HealthURL),
		ManagedProcess: s.deps.control.Probe(spec.Metadata.ControlID),
		Problem:        cause.Error(),
	}
}

func (s *Service) statusForSpec(ctx context.Context, spec TaskSpec) (Status, error) {
	raw, err := s.deps.scheduler.Query(ctx, spec.Name)
	if errors.Is(err, ErrNotInstalled) {
		return Status{TaskName: spec.Name}, ErrNotInstalled
	}
	if err != nil {
		return Status{TaskName: spec.Name}, fmt.Errorf("query scheduled task: %w", err)
	}
	doc, metadata, err := parseTask(raw)
	if err != nil {
		return s.unverifiedStatus(ctx, spec, err), nil
	}
	return s.statusFromTask(ctx, spec, doc, metadata), nil
}

func (s *Service) requireTask(ctx context.Context, spec TaskSpec) (Status, error) {
	status, err := s.statusForSpec(ctx, spec)
	if err != nil {
		return status, err
	}
	if !status.TaskValid {
		return status, fmt.Errorf("task is not canonical: %s", status.Problem)
	}
	return status, nil
}

func (s *Service) taskName() (string, error) {
	if s.deps.scheduler == nil || s.deps.currentSID == nil {
		return "", ErrUnsupported
	}
	sid, err := s.deps.currentSID()
	if err != nil {
		return "", fmt.Errorf("read current Windows SID: %w", err)
	}
	return taskNameForSID(sid), nil
}

func (s *Service) configPath(given string) (string, error) {
	if given == "" {
		home, err := s.deps.homeDir()
		if err != nil {
			return "", fmt.Errorf("find user home: %w", err)
		}
		given = filepath.Join(home, ".sonda", "sonda.yaml")
	}
	abs, err := filepath.Abs(given)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func (s *Service) runNewGeneration(ctx context.Context, spec TaskSpec, status Status, subject string) (Status, error) {
	before := s.runtimeState(ctx, spec.Metadata)
	if before.generationErr != nil {
		return status, fmt.Errorf("inspect managed process generation before start: %w", before.generationErr)
	}
	if before.healthy || before.controlEvent || before.generationActive {
		return status, fmt.Errorf("refusing to start %s %s before the previous process generation stopped completely (%s)", subject, spec.Name, before.describe())
	}
	if err := s.deps.scheduler.Start(ctx, spec.Name); err != nil {
		return status, fmt.Errorf("start scheduled task: %w", err)
	}
	final, started := s.waitForNewGeneration(ctx, spec.Metadata, before.generation)
	if !started {
		status, _ = s.statusForSpec(ctx, spec)
		return status, fmt.Errorf("%s %s run request did not produce a new managed process generation within %s (%s); Task Scheduler may have ignored the request", subject, spec.Name, s.waitLimit(), final.describeGeneration(before.generation))
	}
	return s.statusForSpec(ctx, spec)
}

func (s *Service) waitForNewGeneration(ctx context.Context, metadata Metadata, previous string) (runtimeState, bool) {
	var state runtimeState
	for i := 0; i < s.deps.waitAttempts; i++ {
		state = s.runtimeState(ctx, metadata)
		if state.generationErr == nil && state.healthy && state.controlEvent && state.generationActive && state.generation != "" && state.generation != previous {
			return state, true
		}
		if !s.waitPoll(ctx) {
			return state, false
		}
	}
	return state, false
}

func (s *Service) waitForStopped(ctx context.Context, metadata Metadata) (runtimeState, bool) {
	var state runtimeState
	for i := 0; i < s.deps.waitAttempts; i++ {
		state = s.runtimeState(ctx, metadata)
		if state.generationErr == nil && !state.healthy && !state.controlEvent && !state.generationActive {
			return state, true
		}
		if !s.waitPoll(ctx) {
			return state, false
		}
	}
	return state, false
}

func (s *Service) runtimeState(ctx context.Context, metadata Metadata) runtimeState {
	generation, active, err := s.deps.control.Generation(metadata.ConfigPath)
	return runtimeState{
		healthy:          s.healthy(ctx, metadata.HealthURL),
		controlEvent:     s.deps.control.Probe(metadata.ControlID),
		generation:       generation,
		generationActive: active,
		generationErr:    err,
	}
}

func (s *Service) waitPoll(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(s.deps.pollInterval):
		return true
	}
}

func (s *Service) waitLimit() time.Duration {
	return time.Duration(s.deps.waitAttempts) * s.deps.pollInterval
}

func (s runtimeState) describe() string {
	generation := "inactive"
	if s.generationActive {
		generation = "active"
	}
	if s.generationErr != nil {
		generation = "unreadable: " + s.generationErr.Error()
	}
	return fmt.Sprintf("health=%t, control_event=%t, process_generation=%s", s.healthy, s.controlEvent, generation)
}

func (s runtimeState) describeGeneration(previous string) string {
	description := s.describe()
	switch {
	case s.generationErr != nil:
		return description
	case s.generation == "":
		return description + ", generation_token=missing"
	case s.generation == previous:
		return description + ", generation_token=unchanged"
	default:
		return description + ", generation_token=changed"
	}
}

func generationPath(configPath string) string {
	return filepath.Clean(configPath) + ".autostart-generation"
}

func (s *Service) healthy(ctx context.Context, endpoint string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := s.deps.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func loopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func healthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err == nil {
		trimmed := strings.Trim(host, "[]")
		if ip := net.ParseIP(trimmed); ip != nil && ip.IsUnspecified() {
			if ip.To4() == nil {
				host = "::1"
			} else {
				host = "127.0.0.1"
			}
			listen = net.JoinHostPort(host, port)
		}
	}
	return (&url.URL{Scheme: "http", Host: listen, Path: "/health"}).String()
}

func fileExists(stat func(string) (os.FileInfo, error), path string) bool {
	_, err := stat(path)
	return err == nil
}

// windowsCommandLine quotes argv using CommandLineToArgvW-compatible rules.
// Task Scheduler stores one argument string, not an argv array.
func windowsCommandLine(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = quoteWindowsArg(arg)
	}
	return strings.Join(quoted, " ")
}

func quoteWindowsArg(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var out strings.Builder
	out.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		switch r {
		case '\\':
			backslashes++
		case '"':
			out.WriteString(strings.Repeat("\\", backslashes*2+1))
			out.WriteRune(r)
			backslashes = 0
		default:
			out.WriteString(strings.Repeat("\\", backslashes))
			backslashes = 0
			out.WriteRune(r)
		}
	}
	out.WriteString(strings.Repeat("\\", backslashes*2))
	out.WriteByte('"')
	return out.String()
}
