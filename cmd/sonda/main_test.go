package main

import (
	"strings"
	"testing"
)

// version() is what a user runs to find out what they downloaded, and it is the
// one thing in the release path that a build can get wrong silently: an empty
// tag turning into a double space, or a release binary that never mentions the
// tag it was cut from.

func TestVersionReportsTheReleaseTagWhenThereIsOne(t *testing.T) {
	t.Cleanup(func() { release = "" })
	release = "v1.2.3"

	got := version()
	if !strings.Contains(got, "v1.2.3") {
		t.Errorf("version() = %q, does not mention the tag it was built from", got)
	}
	if !strings.HasPrefix(got, "sonda v1.2.3 ") {
		t.Errorf("version() = %q, want the tag right after the name", got)
	}
}

// A plain `go build` sets no tag. Saying nothing is correct — claiming a
// version it was not cut from would not be — but it must not leave the gap
// behind as ragged spacing.
func TestVersionWithoutATagIsStillWellFormed(t *testing.T) {
	t.Cleanup(func() { release = "" })
	release = ""

	got := version()
	if strings.Contains(got, "  ") {
		t.Errorf("version() = %q, has a double space where the tag would go", got)
	}
	if strings.Contains(got, " v") {
		t.Errorf("version() = %q, reports a tag when none was set", got)
	}
	if !strings.HasPrefix(got, "sonda ") {
		t.Errorf("version() = %q, want it to start with the binary name", got)
	}
}
