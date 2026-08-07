package main

import (
	"runtime/debug"
	"strings"
	"testing"
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
