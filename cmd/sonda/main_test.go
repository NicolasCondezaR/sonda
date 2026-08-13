package main

import (
	"bytes"
	"context"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/NicolasCondezaR/sonda/internal/autostart"
)

// version() is what a user runs to find out what they downloaded, and the four
// ways a Sonda binary comes into existence each know a different amount about
// themselves. Getting this wrong is silent: a binary that reports nothing looks
// the same as one that reports the wrong thing.
//
//	release archive   ldflags carry the tag, .git is there
//	go install        no ldflags; Go records the module version it fetched
//	go build          no ldflags, no module version, but .git is there
//	container image   ldflags carry the tag, .git deliberately is not

func info(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: "go1.26.0",
		Main:      debug.Module{Version: mainVersion},
		Settings:  settings,
	}
}

func vcs(revision string, modified bool) []debug.BuildSetting {
	out := []debug.BuildSetting{{Key: "vcs.revision", Value: revision}}
	if modified {
		out = append(out, debug.BuildSetting{Key: "vcs.modified", Value: "true"})
	}
	return out
}

func TestVersionFromAReleaseArchive(t *testing.T) {
	got := formatVersion("v1.2.3", info("v1.2.3", vcs("abcdef0123456789", false)...))
	want := "sonda v1.2.3 abcdef012345 go1.26.0"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The tag has to survive `go install`, which never sees ldflags. Without the
// module version there is nothing at all to report, and this is the path the
// README puts first.
func TestVersionFromGoInstall(t *testing.T) {
	got := formatVersion("", info("v1.2.3"))
	if !strings.Contains(got, "v1.2.3") {
		t.Errorf("got %q, does not report the module version it was installed from", got)
	}
}

// A local build knows its commit but has no version, and "(devel)" is what Go
// puts there. Reporting it verbatim would look like a release called devel.
func TestVersionFromALocalBuild(t *testing.T) {
	got := formatVersion("", info("(devel)", vcs("abcdef0123456789", true)...))

	if strings.Contains(got, "devel") {
		t.Errorf("got %q, leaks Go's placeholder version", got)
	}
	if !strings.Contains(got, "(uncommitted changes)") {
		t.Errorf("got %q, does not warn that the tree was dirty", got)
	}
	if !strings.Contains(got, "abcdef012345") {
		t.Errorf("got %q, does not report the commit", got)
	}
}

// The image is built from a context without .git, on purpose: that is what
// keeps the build context at kilobytes. Saying so beats implying a revision.
func TestVersionFromTheContainerImage(t *testing.T) {
	got := formatVersion("v1.2.3", info(""))
	want := "sonda v1.2.3 (built without VCS information) go1.26.0"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Both sources can be present at once and they can disagree — ldflags is the
// one the release actually cut, so it wins.
func TestTheReleaseTagBeatsTheModuleVersion(t *testing.T) {
	got := formatVersion("v2.0.0", info("v1.2.3", vcs("abcdef0123456789", false)...))
	if !strings.Contains(got, "v2.0.0") || strings.Contains(got, "v1.2.3") {
		t.Errorf("got %q, want the ldflags tag to win", got)
	}
}

// Nothing known at all still has to read like a sentence, not like a gap.
func TestVersionWithNoBuildInfoAtAll(t *testing.T) {
	got := formatVersion("", nil)
	if strings.Contains(got, "  ") {
		t.Errorf("got %q, has a double space where a field would go", got)
	}
	if !strings.HasPrefix(got, "sonda ") {
		t.Errorf("got %q, want it to start with the binary name", got)
	}
}

type fakeAutostartCLI struct {
	command string
	options autostart.InstallOptions
	manage  autostart.ManageOptions
	status  autostart.Status
	err     error
}

func (f *fakeAutostartCLI) Install(_ context.Context, options autostart.InstallOptions) (autostart.Status, error) {
	f.command, f.options = "install", options
	return f.status, f.err
}
func (f *fakeAutostartCLI) Status(_ context.Context, options autostart.ManageOptions) (autostart.Status, error) {
	f.command, f.manage = "status", options
	return f.status, f.err
}
func (f *fakeAutostartCLI) Start(_ context.Context, options autostart.ManageOptions) (autostart.Status, error) {
	f.command, f.manage = "start", options
	return f.status, f.err
}
func (f *fakeAutostartCLI) Stop(_ context.Context, options autostart.ManageOptions) (autostart.Status, error) {
	f.command, f.manage = "stop", options
	return f.status, f.err
}
func (f *fakeAutostartCLI) Restart(_ context.Context, options autostart.ManageOptions) (autostart.Status, error) {
	f.command, f.manage = "restart", options
	return f.status, f.err
}
func (f *fakeAutostartCLI) Uninstall(_ context.Context, options autostart.ManageOptions) (autostart.Status, error) {
	f.command, f.manage = "uninstall", options
	return f.status, f.err
}

func TestAutostartCLICommands(t *testing.T) {
	commands := []string{"status", "start", "stop", "restart", "uninstall"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			service := &fakeAutostartCLI{status: autostart.Status{TaskName: "Sonda-test"}}
			var stdout, stderr bytes.Buffer
			if err := runAutostartWith(context.Background(), service, []string{command}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if service.command != command || !strings.Contains(stdout.String(), "task: Sonda-test") {
				t.Fatalf("command=%q output=%q", service.command, stdout.String())
			}
		})
	}
}

func TestAutostartInstallParsesSecurityOverrideExplicitly(t *testing.T) {
	service := &fakeAutostartCLI{}
	var stdout, stderr bytes.Buffer
	err := runAutostartWith(context.Background(), service, []string{
		"install", "-config", `C:\Users\Jane Doe\.sonda\sonda.yaml`, "-allow-non-loopback",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if service.command != "install" || service.options.ConfigPath == "" || !service.options.AllowNonLoopback {
		t.Fatalf("command=%q options=%+v", service.command, service.options)
	}
}

func TestAutostartLifecycleCommandsBindAnExplicitConfig(t *testing.T) {
	service := &fakeAutostartCLI{status: autostart.Status{TaskName: "Sonda-test"}}
	var stdout, stderr bytes.Buffer
	want := `C:\Users\Jane Doe\.sonda\custom.yaml`
	if err := runAutostartWith(context.Background(), service, []string{"stop", "-config", want}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if service.command != "stop" || service.manage.ConfigPath != want {
		t.Fatalf("command=%q options=%+v", service.command, service.manage)
	}
}

func TestAutostartCLIRejectsUnknownCommandsAndArguments(t *testing.T) {
	tests := [][]string{{"unknown"}, {"status", "extra"}, {"install", "extra"}}
	for _, argv := range tests {
		t.Run(strings.Join(argv, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := runAutostartWith(context.Background(), &fakeAutostartCLI{}, argv, &stdout, &stderr); err == nil {
				t.Fatalf("argv %v succeeded", argv)
			}
		})
	}
}
