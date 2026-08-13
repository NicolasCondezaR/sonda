package autostart

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeScheduler struct {
	mu                  sync.Mutex
	raw                 []byte
	queryCalls          int
	registerCalls       int
	startCalls          int
	endCalls            int
	deleteCalls         int
	lastTask            string
	onStart             func()
	onEnd               func()
	onQuery             func(int)
	onRegister          func()
	normalizeRegistered func([]byte) []byte
}

func (f *fakeScheduler) Query(_ context.Context, task string) ([]byte, error) {
	f.mu.Lock()
	if f.raw == nil {
		f.mu.Unlock()
		return nil, ErrNotInstalled
	}
	f.queryCalls++
	f.lastTask = task
	raw := append([]byte(nil), f.raw...)
	hook, calls := f.onQuery, f.queryCalls
	f.mu.Unlock()
	if hook != nil {
		hook(calls)
	}
	f.mu.Lock()
	raw = append(raw[:0], f.raw...)
	f.mu.Unlock()
	return raw, nil
}

func (f *fakeScheduler) Register(_ context.Context, task string, raw []byte) error {
	if f.onRegister != nil {
		f.onRegister()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.raw != nil {
		return errors.New("task name already exists")
	}
	if f.normalizeRegistered != nil {
		raw = f.normalizeRegistered(raw)
	}
	f.raw = append([]byte(nil), raw...)
	f.registerCalls++
	f.lastTask = task
	return nil
}

func (f *fakeScheduler) Start(_ context.Context, task string) error {
	f.mu.Lock()
	f.startCalls++
	f.lastTask = task
	hook := f.onStart
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeScheduler) End(_ context.Context, task string) error {
	f.mu.Lock()
	f.endCalls++
	f.lastTask = task
	hook := f.onEnd
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeScheduler) Delete(_ context.Context, task string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw = nil
	f.deleteCalls++
	f.lastTask = task
	return nil
}

func (f *fakeScheduler) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCalls = 0
	f.startCalls = 0
	f.endCalls = 0
	f.deleteCalls = 0
	f.queryCalls = 0
	f.lastTask = ""
}

func (f *fakeScheduler) calls() (register, start, end, delete int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerCalls, f.startCalls, f.endCalls, f.deleteCalls
}

type fakeControl struct {
	mu               sync.Mutex
	running          bool
	signals          int
	signalErr        error
	onSignal         func()
	generation       string
	generationActive bool
	generationErr    error
	generationCalls  int
	nextGeneration   int
	onGeneration     func(int)
}

func (f *fakeControl) Probe(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeControl) Signal(string) error {
	f.mu.Lock()
	f.signals++
	err, hook := f.signalErr, f.onSignal
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (f *fakeControl) Generation(string) (string, bool, error) {
	f.mu.Lock()
	f.generationCalls++
	calls, hook := f.generationCalls, f.onGeneration
	f.mu.Unlock()
	if hook != nil {
		hook(calls)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation, f.generationActive, f.generationErr
}

func (f *fakeControl) signalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signals
}

func (f *fakeControl) resetSignals() {
	f.mu.Lock()
	f.signals = 0
	f.mu.Unlock()
}

func (f *fakeControl) setRunning(value bool) {
	f.mu.Lock()
	f.running = value
	f.mu.Unlock()
}

func (f *fakeControl) startGeneration() {
	f.mu.Lock()
	f.nextGeneration++
	f.generation = fmt.Sprintf("generation-%d", f.nextGeneration)
	f.generationActive = true
	f.mu.Unlock()
}

func (f *fakeControl) stopGeneration() {
	f.mu.Lock()
	f.generationActive = false
	f.mu.Unlock()
}

func (f *fakeControl) setGeneration(token string, active bool) {
	f.mu.Lock()
	f.generation = token
	f.generationActive = active
	f.mu.Unlock()
}

func (f *fakeControl) resetGenerationCalls() {
	f.mu.Lock()
	f.generationCalls = 0
	f.onGeneration = nil
	f.mu.Unlock()
}

func (f *fakeControl) generationSnapshot() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation, f.generationActive
}

type healthTransport struct {
	mu      sync.Mutex
	healthy bool
}

func (h *healthTransport) RoundTrip(*http.Request) (*http.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.healthy {
		return nil, errors.New("offline")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
		Header:     make(http.Header),
	}, nil
}

func (h *healthTransport) set(value bool) {
	h.mu.Lock()
	h.healthy = value
	h.mu.Unlock()
}

type serviceFixture struct {
	service   *Service
	scheduler *fakeScheduler
	control   *fakeControl
	health    *healthTransport
	config    string
	launcher  string
}

func newServiceFixture(t *testing.T) serviceFixture {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".sonda", "sonda.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("api_listen: 127.0.0.1:19000\ndatabase: sonda.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, "bin", "sonda.exe")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}

	scheduler := &fakeScheduler{}
	control := &fakeControl{}
	health := &healthTransport{}
	scheduler.onStart = func() {
		control.startGeneration()
		control.setRunning(true)
		health.set(true)
	}
	scheduler.onEnd = func() {
		control.stopGeneration()
		control.setRunning(false)
		health.set(false)
	}
	control.onSignal = scheduler.onEnd

	service := newService(dependencies{
		scheduler: scheduler,
		control:   control,
		currentSID: func() (string, error) {
			return "S-1-5-21-1000", nil
		},
		resolveAccountSID: func(account string) (string, error) {
			if strings.EqualFold(account, `LAPTOP-TEST\User`) {
				return "S-1-5-21-1000", nil
			}
			if strings.EqualFold(account, `LAPTOP-TEST\Other`) {
				return "S-1-5-21-2000", nil
			}
			return "", errors.New("unknown account")
		},
		resolveLauncher: func() (Launcher, error) {
			return Launcher{Path: launcher, Portable: true, Warning: "portable"}, nil
		},
		homeDir:      func() (string, error) { return dir, nil },
		httpClient:   &http.Client{Transport: health},
		waitAttempts: 2,
		pollInterval: time.Nanosecond,
	})
	return serviceFixture{service: service, scheduler: scheduler, control: control, health: health, config: configPath, launcher: launcher}
}

func TestInstallIsIdempotentAndStartsTheManagedTask(t *testing.T) {
	fx := newServiceFixture(t)
	ctx := context.Background()

	first, err := fx.service.Install(ctx, InstallOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.service.Install(ctx, InstallOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Installed || !first.Healthy || !first.ManagedProcess || !second.TaskValid {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if fx.scheduler.registerCalls != 1 || fx.scheduler.startCalls != 1 {
		t.Fatalf("register=%d start=%d, want one each", fx.scheduler.registerCalls, fx.scheduler.startCalls)
	}
}

func TestInstallStatusAndStartAcceptSchedulerAccountNameNormalization(t *testing.T) {
	fx := newServiceFixture(t)
	resolveCalls := 0
	fx.service.deps.resolveAccountSID = func(account string) (string, error) {
		resolveCalls++
		if strings.EqualFold(account, `LAPTOP-TEST\User`) {
			return "S-1-5-21-1000", nil
		}
		return "", errors.New("unknown account")
	}
	fx.scheduler.normalizeRegistered = func(raw []byte) []byte {
		return mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
			doc.Triggers.Logon.UserID = `LAPTOP-TEST\User`
			doc.RegistrationInfo.Author = `LAPTOP-TEST\User`
		})(t, &fx, raw)
	}

	installed, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if !installed.TaskValid || !installed.Healthy || !installed.ManagedProcess {
		t.Fatalf("installed=%+v", installed)
	}
	if fx.scheduler.queryCalls != 2 {
		t.Fatalf("post-register queries=%d, want exactly two consecutive canonical checks", fx.scheduler.queryCalls)
	}
	if resolveCalls != 4 {
		t.Fatalf("identity resolutions=%d, want stable trigger+author resolution on both queries", resolveCalls)
	}

	status, err := fx.service.Status(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil || !status.TaskValid {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	fx.control.setRunning(false)
	fx.control.stopGeneration()
	fx.health.set(false)
	started, err := fx.service.Start(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if !started.TaskValid || !started.Healthy || !started.ManagedProcess {
		t.Fatalf("started=%+v", started)
	}
}

func TestSchedulerAccountNameMustResolveToTheCurrentSID(t *testing.T) {
	tests := []struct {
		name    string
		account string
		valid   bool
	}{
		{name: "current account", account: `LAPTOP-TEST\User`, valid: true},
		{name: "different account", account: `LAPTOP-TEST\Other`, valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newServiceFixture(t)
			raw := mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.Triggers.Logon.UserID = tt.account
			})(t, &fx, canonicalTaskBytes(t, &fx))
			fx.scheduler.raw = raw
			status, err := fx.service.Status(context.Background(), ManageOptions{ConfigPath: fx.config})
			if err != nil {
				t.Fatal(err)
			}
			if status.TaskValid != tt.valid {
				t.Fatalf("status=%+v, want valid=%t", status, tt.valid)
			}
		})
	}
}

func TestRealSchedulerXMLFixtureIsCanonicalAcrossConsecutiveChecks(t *testing.T) {
	const (
		sid      = "S-1-5-21-3679465909-2745701859-1701774546-1001"
		taskName = "Sonda-33416a2e3f8d"
		account  = `LAPTOP-RABN9KTL\User`
		config   = `C:\Users\User\.sonda\sonda.yaml`
		logPath  = `C:\Users\User\.sonda\sonda.log`
		launcher = `C:\Users\User\.sonda\bin\sonda.exe`
		workDir  = `C:\Users\User\.sonda`
		control  = "Sonda-33416a2e3f8d-6c60a8359fc4"
	)
	metadata := Metadata{
		Version: 1, ConfigPath: config, LogPath: logPath, ControlID: control,
		HealthURL: "http://127.0.0.1:9000/health", PortableLauncher: true,
	}
	documentation, err := encodeMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	arguments := windowsCommandLine([]string{
		"-config", config,
		"-log-file", logPath,
		"-autostart-control", control,
	})
	raw := []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="%s">
  <RegistrationInfo>
    <Author>Sonda</Author>
    <Description>Starts Sonda for this user at logon. Managed by 'sonda autostart'.</Description>
    <URI>\%s</URI>
    <Documentation>%s</Documentation>
  </RegistrationInfo>
  <Principals><Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType></Principal></Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <RestartOnFailure><Count>3</Count><Interval>PT1M</Interval></RestartOnFailure>
    <StartWhenAvailable>true</StartWhenAvailable>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
  </Settings>
  <Triggers><LogonTrigger><UserId>%s</UserId></LogonTrigger></Triggers>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>`, taskNamespace, taskName, documentation, sid, account, launcher, arguments, workDir))
	doc, parsedMetadata, err := parseTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{
		Name: taskName, SID: sid, Launcher: launcher, Arguments: arguments,
		WorkingDirectory: workDir, Metadata: metadata,
	}
	resolveCalls := 0
	service := newService(dependencies{resolveAccountSID: func(value string) (string, error) {
		resolveCalls++
		if value != account {
			return "", errors.New("unexpected account")
		}
		return sid, nil
	}})
	for query := 1; query <= 2; query++ {
		if mismatch := service.taskMismatchCurrentUser(spec, doc, parsedMetadata); mismatch != "" {
			t.Fatalf("query %d mismatch=%s", query, mismatch)
		}
	}
	if resolveCalls != 2 {
		t.Fatalf("resolve calls=%d, want stable trigger resolution for two queries", resolveCalls)
	}
}

func TestCanonicalValidationReportsTheExactSafeField(t *testing.T) {
	fx := newServiceFixture(t)
	spec, _, _, err := fx.service.canonicalTask(fx.config, false)
	if err != nil {
		t.Fatal(err)
	}
	doc, metadata, err := parseTask(canonicalTaskBytes(t, &fx))
	if err != nil {
		t.Fatal(err)
	}
	doc.Actions.Exec.Arguments += " -unexpected"
	valid, problem := fx.service.validateTask(spec, doc, metadata)
	if valid || problem != "canonical field mismatch: action.arguments" {
		t.Fatalf("valid=%t problem=%q", valid, problem)
	}
}

func TestInstallDoesNotOverwriteATaskCreatedAfterTheAbsenceCheck(t *testing.T) {
	fx := newServiceFixture(t)
	foreign := []byte(`<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><Author>Other</Author></RegistrationInfo></Task>`)
	fx.scheduler.onRegister = func() {
		fx.scheduler.mu.Lock()
		fx.scheduler.raw = append([]byte(nil), foreign...)
		fx.scheduler.mu.Unlock()
	}

	_, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "register scheduled task") {
		t.Fatalf("error = %v", err)
	}
	register, start, end, deleteCalls := fx.scheduler.calls()
	if register != 0 || start != 0 || end != 0 || deleteCalls != 0 {
		t.Fatalf("register=%d start=%d end=%d delete=%d", register, start, end, deleteCalls)
	}
	fx.scheduler.mu.Lock()
	got := append([]byte(nil), fx.scheduler.raw...)
	fx.scheduler.mu.Unlock()
	if string(got) != string(foreign) {
		t.Fatalf("foreign task was overwritten: %s", got)
	}
}

func TestInstallReturnsTheFreshCanonicalFieldMismatch(t *testing.T) {
	fx := newServiceFixture(t)
	fx.scheduler.normalizeRegistered = func(raw []byte) []byte {
		return mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
			doc.Settings.StartWhenAvailable = false
		})(t, &fx, raw)
	}

	status, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "settings.start_when_available") {
		t.Fatalf("error=%v", err)
	}
	if status.TaskValid || status.Problem != "canonical field mismatch: settings.start_when_available" {
		t.Fatalf("status=%+v", status)
	}
	if _, start, _, _ := fx.scheduler.calls(); start != 0 {
		t.Fatalf("start calls=%d", start)
	}
}

func TestInstallRefusesNonLoopbackWithoutExplicitOverride(t *testing.T) {
	fx := newServiceFixture(t)
	if err := os.WriteFile(fx.config, []byte("api_listen: 0.0.0.0:19000\ndatabase: sonda.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("error = %v", err)
	}
	if fx.scheduler.registerCalls != 0 {
		t.Fatal("task was registered before the security check")
	}

	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config, AllowNonLoopback: true}); err != nil {
		t.Fatalf("explicit override failed: %v", err)
	}
}

func TestStatusDistinguishesMissingTaskAndMissingLauncher(t *testing.T) {
	fx := newServiceFixture(t)
	status, err := fx.service.Status(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil || status.Installed {
		t.Fatalf("absent status=%+v err=%v", status, err)
	}
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fx.launcher); err != nil {
		t.Fatal(err)
	}
	status, err = fx.service.Status(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.TaskValid || !strings.Contains(status.Problem, "launcher is missing") {
		t.Fatalf("status=%+v", status)
	}
}

func TestStopUsesTheManagedEventBeforeTaskSchedulerFallback(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	status, err := fx.service.Stop(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy || status.ManagedProcess || status.AbruptStop {
		t.Fatalf("status=%+v", status)
	}
	if fx.scheduler.endCalls != 0 {
		t.Fatal("Task Scheduler ended a process even though graceful control worked")
	}
}

func TestStopFallsBackOnlyToTheOwnedTask(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	fx.control.signalErr = ErrNotRunning
	fx.control.onSignal = nil
	status, err := fx.service.Stop(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if !status.AbruptStop || fx.scheduler.endCalls != 1 || fx.scheduler.lastTask != taskNameForSID("S-1-5-21-1000") {
		t.Fatalf("status=%+v end=%d task=%q", status, fx.scheduler.endCalls, fx.scheduler.lastTask)
	}
}

func TestRestartWaitsForTheOldGenerationToExitBeforeRun(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	oldGeneration, active := fx.control.generationSnapshot()
	if oldGeneration == "" || !active {
		t.Fatalf("installed generation=%q active=%t", oldGeneration, active)
	}

	fx.service.deps.waitAttempts = 6
	fx.control.resetGenerationCalls()
	fx.control.onSignal = func() {
		// The old process is draining. Its health listener, control event, and
		// process-lifetime lease are deliberately still active.
	}
	fx.control.onGeneration = func(calls int) {
		switch calls {
		case 2:
			// Listener and event close before the process itself exits.
			fx.health.set(false)
			fx.control.setRunning(false)
		case 3:
			// Windows releases the generation lease only at process exit.
			fx.control.stopGeneration()
		}
	}
	runWhileOldActive := false
	fx.scheduler.onStart = func() {
		_, oldActive := fx.control.generationSnapshot()
		if oldActive {
			runWhileOldActive = true
			return // IgnoreNew would discard this run request.
		}
		fx.control.startGeneration()
		fx.control.setRunning(true)
		fx.health.set(true)
	}

	status, err := fx.service.Restart(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, active := fx.control.generationSnapshot()
	if runWhileOldActive || newGeneration == oldGeneration || !active {
		t.Fatalf("runWhileOldActive=%t old=%q new=%q active=%t status=%+v", runWhileOldActive, oldGeneration, newGeneration, active, status)
	}
	if !status.Healthy || !status.ManagedProcess || fx.scheduler.startCalls != 2 {
		t.Fatalf("status=%+v start calls=%d", status, fx.scheduler.startCalls)
	}
}

func TestStopFailsPreciselyWhileAProcessGenerationIsStillDraining(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	fx.control.setRunning(false)
	fx.health.set(false)

	status, err := fx.service.Stop(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "previous process generation did not exit within 2ns") || !strings.Contains(err.Error(), "process_generation=active") {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	if fx.scheduler.endCalls != 0 || fx.scheduler.startCalls != 1 {
		t.Fatalf("start=%d end=%d", fx.scheduler.startCalls, fx.scheduler.endCalls)
	}
}

func TestStartFailsWhenRunIsIgnoredAndOnlyOldSignalsReappear(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.service.Stop(context.Background(), ManageOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	oldGeneration, active := fx.control.generationSnapshot()
	if oldGeneration == "" || active {
		t.Fatalf("stopped generation=%q active=%t", oldGeneration, active)
	}

	fx.scheduler.onStart = func() {
		// Model an accepted-looking /Run that was ignored: stale observable
		// signals may be present, but no new process owns the lifetime lease.
		fx.control.setRunning(true)
		fx.health.set(true)
	}
	status, err := fx.service.Start(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "did not produce a new managed process generation") || !strings.Contains(err.Error(), "generation_token=unchanged") || !strings.Contains(err.Error(), "within 2ns") {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	if fx.scheduler.startCalls != 2 {
		t.Fatalf("start calls=%d", fx.scheduler.startCalls)
	}
}

func TestStartWaitsForANewGenerationHandshake(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.service.Stop(context.Background(), ManageOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	oldGeneration, _ := fx.control.generationSnapshot()

	fx.service.deps.waitAttempts = 4
	fx.control.resetGenerationCalls()
	fx.scheduler.onStart = func() {
		fx.control.setRunning(true)
		fx.health.set(true)
	}
	fx.control.onGeneration = func(calls int) {
		if calls == 3 {
			fx.control.startGeneration()
		}
	}

	status, err := fx.service.Start(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, active := fx.control.generationSnapshot()
	if newGeneration == oldGeneration || !active || !status.Healthy || !status.ManagedProcess {
		t.Fatalf("old=%q new=%q active=%t status=%+v", oldGeneration, newGeneration, active, status)
	}
}

func TestUninstallPreservesEveryUserFile(t *testing.T) {
	fx := newServiceFixture(t)
	stateDir := filepath.Dir(fx.config)
	for _, name := range []string{"sonda.db", "sonda-ca.key", "sonda.log"} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	status, err := fx.service.Uninstall(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err != nil {
		t.Fatal(err)
	}
	if status.Installed || fx.scheduler.deleteCalls != 1 {
		t.Fatalf("status=%+v deletes=%d", status, fx.scheduler.deleteCalls)
	}
	for _, name := range []string{"sonda.yaml", "sonda.db", "sonda-ca.key", "sonda.log"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

func TestInstallWillNotClaimAnAddressOwnedByAnotherProcess(t *testing.T) {
	fx := newServiceFixture(t)
	fx.health.set(true)
	_, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("error = %v", err)
	}
	if fx.scheduler.registerCalls != 0 || fx.scheduler.startCalls != 0 {
		t.Fatalf("register=%d start=%d", fx.scheduler.registerCalls, fx.scheduler.startCalls)
	}
}

func TestStopWillNotTouchAnUnlinkedHealthyProcess(t *testing.T) {
	fx := newServiceFixture(t)
	if _, err := fx.service.Install(context.Background(), InstallOptions{ConfigPath: fx.config}); err != nil {
		t.Fatal(err)
	}
	fx.control.setRunning(false)
	fx.health.set(true)
	_, err := fx.service.Stop(context.Background(), ManageOptions{ConfigPath: fx.config})
	if err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Fatalf("error = %v", err)
	}
	if fx.scheduler.endCalls != 0 {
		t.Fatal("an unlinked process caused Task Scheduler fallback")
	}
}

func TestForeignAndMutatedTasksAreNeverOperatedOn(t *testing.T) {
	type mutation struct {
		name  string
		apply func(*testing.T, *serviceFixture, []byte) []byte
	}
	mutations := []mutation{
		{
			name: "foreign task without Sonda metadata",
			apply: func(_ *testing.T, _ *serviceFixture, _ []byte) []byte {
				return []byte(`<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><Author>Other</Author></RegistrationInfo></Task>`)
			},
		},
		{
			name: "task identity URI",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.RegistrationInfo.URI = `\ForeignTask`
			}),
		},
		{
			name: "principal SID",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.Principals.Principal.UserID = "S-1-5-21-2000"
			}),
		},
		{
			name: "trigger SID",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.Triggers.Logon.UserID = "S-1-5-21-2000"
			}),
		},
		{
			name: "principal and trigger SID",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.Principals.Principal.UserID = "S-1-5-21-2000"
				doc.Triggers.Logon.UserID = "S-1-5-21-2000"
			}),
		},
		{
			name: "config binding",
			apply: mutateTask(func(t *testing.T, fx *serviceFixture, doc *taskDocument, metadata *Metadata) {
				foreignConfig := filepath.Join(filepath.Dir(fx.config), "foreign.yaml")
				if err := os.WriteFile(foreignConfig, []byte("api_listen: 127.0.0.1:19001\ndatabase: foreign.db\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				metadata.ConfigPath = foreignConfig
				metadata.ControlID = controlID(taskNameForSID("S-1-5-21-1000"), foreignConfig)
				metadata.HealthURL = "http://127.0.0.1:19001/health"
				doc.Actions.Exec.Arguments = windowsCommandLine([]string{
					"-config", metadata.ConfigPath,
					"-log-file", metadata.LogPath,
					"-autostart-control", metadata.ControlID,
				})
			}),
		},
		{
			name: "control ID",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, metadata *Metadata) {
				metadata.ControlID = "foreign-control"
				doc.Actions.Exec.Arguments = windowsCommandLine([]string{
					"-config", metadata.ConfigPath,
					"-log-file", metadata.LogPath,
					"-autostart-control", metadata.ControlID,
				})
			}),
		},
		{
			name: "health URL",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, _ *taskDocument, metadata *Metadata) {
				metadata.HealthURL = "http://127.0.0.1:19001/health"
			}),
		},
		{
			name: "action launcher",
			apply: mutateTask(func(t *testing.T, fx *serviceFixture, doc *taskDocument, _ *Metadata) {
				foreignLauncher := filepath.Join(filepath.Dir(fx.launcher), "foreign.exe")
				if err := os.WriteFile(foreignLauncher, []byte("foreign"), 0o700); err != nil {
					t.Fatal(err)
				}
				doc.Actions.Exec.Command = foreignLauncher
			}),
		},
		{
			name: "action arguments",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.Actions.Exec.Arguments += " -unexpected"
			}),
		},
		{
			name: "safety settings",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, doc *taskDocument, _ *Metadata) {
				doc.Settings.Priority = intPointer(1)
			}),
		},
		{
			name: "metadata",
			apply: mutateTask(func(_ *testing.T, _ *serviceFixture, _ *taskDocument, metadata *Metadata) {
				metadata.PortableLauncher = false
			}),
		},
	}

	type operation struct {
		name string
		run  func(context.Context, *Service, string) error
	}
	operations := []operation{
		{
			name: "install",
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Install(ctx, InstallOptions{ConfigPath: configPath})
				return err
			},
		},
		{
			name: "start",
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Start(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
		{
			name: "stop",
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Stop(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
		{
			name: "restart",
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Restart(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
		{
			name: "uninstall",
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Uninstall(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
	}

	for _, change := range mutations {
		t.Run(change.name+" status", func(t *testing.T) {
			fx := newServiceFixture(t)
			raw := canonicalTaskBytes(t, &fx)
			fx.scheduler.raw = change.apply(t, &fx, raw)
			status, err := fx.service.Status(context.Background(), ManageOptions{ConfigPath: fx.config})
			if err != nil {
				t.Fatal(err)
			}
			if !status.Installed || status.TaskValid || status.Problem == "" {
				t.Fatalf("status=%+v", status)
			}
		})

		for _, action := range operations {
			t.Run(change.name+" "+action.name, func(t *testing.T) {
				fx := newServiceFixture(t)
				raw := canonicalTaskBytes(t, &fx)
				fx.scheduler.raw = change.apply(t, &fx, raw)
				fx.scheduler.resetCalls()
				fx.control.resetSignals()
				if action.name == "stop" || action.name == "restart" || action.name == "uninstall" {
					fx.control.setRunning(true)
					fx.health.set(true)
				}

				if err := action.run(context.Background(), fx.service, fx.config); err == nil {
					t.Fatal("operation succeeded for an unverified task")
				}
				register, start, end, deleteCalls := fx.scheduler.calls()
				if register != 0 || start != 0 || end != 0 || deleteCalls != 0 || fx.control.signalCalls() != 0 {
					t.Fatalf("register=%d start=%d signal=%d end=%d delete=%d", register, start, fx.control.signalCalls(), end, deleteCalls)
				}
			})
		}
	}
}

func TestTaskMutationBetweenValidationAndSideEffectFailsClosed(t *testing.T) {
	operations := []struct {
		name          string
		mutateOnQuery int
		running       bool
		run           func(context.Context, *Service, string) error
	}{
		{
			name: "start", mutateOnQuery: 2,
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Start(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
		{
			name: "scheduler end fallback", mutateOnQuery: 2, running: true,
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Stop(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
		{
			name: "delete", mutateOnQuery: 2,
			run: func(ctx context.Context, service *Service, configPath string) error {
				_, err := service.Uninstall(ctx, ManageOptions{ConfigPath: configPath})
				return err
			},
		},
	}

	for _, action := range operations {
		t.Run(action.name, func(t *testing.T) {
			fx := newServiceFixture(t)
			fx.scheduler.raw = canonicalTaskBytes(t, &fx)
			fx.scheduler.resetCalls()
			if action.running {
				fx.control.setRunning(true)
				fx.health.set(true)
				fx.control.signalErr = ErrNotRunning
				fx.control.onSignal = nil
			}
			if action.name == "delete" {
				fx.control.setRunning(false)
				fx.health.set(false)
			}
			fx.scheduler.onQuery = func(calls int) {
				if calls != action.mutateOnQuery {
					return
				}
				fx.scheduler.mu.Lock()
				doc, metadata, err := parseTask(fx.scheduler.raw)
				if err != nil {
					fx.scheduler.mu.Unlock()
					t.Fatal(err)
				}
				doc.Settings.Priority = intPointer(1)
				doc.RegistrationInfo.Documentation, err = encodeMetadata(metadata)
				if err != nil {
					fx.scheduler.mu.Unlock()
					t.Fatal(err)
				}
				encoded, err := xml.MarshalIndent(doc, "", "  ")
				if err != nil {
					fx.scheduler.mu.Unlock()
					t.Fatal(err)
				}
				fx.scheduler.raw = append([]byte(xml.Header), encoded...)
				fx.scheduler.mu.Unlock()
			}

			if err := action.run(context.Background(), fx.service, fx.config); err == nil {
				t.Fatal("operation succeeded after task ownership changed")
			}
			register, start, end, deleteCalls := fx.scheduler.calls()
			if register != 0 || start != 0 || end != 0 || deleteCalls != 0 {
				t.Fatalf("register=%d start=%d end=%d delete=%d", register, start, end, deleteCalls)
			}
		})
	}
}

func canonicalTaskBytes(t *testing.T, fx *serviceFixture) []byte {
	t.Helper()
	spec, _, _, err := fx.service.canonicalTask(fx.config, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := marshalTask(spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutateTask(mutate func(*testing.T, *serviceFixture, *taskDocument, *Metadata)) func(*testing.T, *serviceFixture, []byte) []byte {
	return func(t *testing.T, fx *serviceFixture, raw []byte) []byte {
		t.Helper()
		doc, metadata, err := parseTask(raw)
		if err != nil {
			t.Fatal(err)
		}
		mutate(t, fx, &doc, &metadata)
		doc.RegistrationInfo.Documentation, err = encodeMetadata(metadata)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := xml.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append([]byte(xml.Header), encoded...)
	}
}
