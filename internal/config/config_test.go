package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
targets:
  - name: api
    listen: 127.0.0.1:9101
    upstream: http://127.0.0.1:3000
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIListen != defaultAPIListen {
		t.Errorf("api_listen = %q", c.APIListen)
	}
	if c.MaxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("max_body_bytes = %d", c.MaxBodyBytes)
	}
	if c.Targets[0].Protocol != ProtocolHTTP {
		t.Errorf("protocol = %q, want the http default", c.Targets[0].Protocol)
	}
	if c.Retention.MaxAgeDuration() != 24*time.Hour {
		t.Errorf("max_age = %v", c.Retention.MaxAgeDuration())
	}
	if c.Retention.IntervalDuration() != time.Minute {
		t.Errorf("interval = %v", c.Retention.IntervalDuration())
	}
}

// A file with no targets is an ordinary first run now, not an error: projects
// live in the database and the interface fills them in.
func TestAConfigWithNoTargetsIsValid(t *testing.T) {
	c, err := Parse([]byte("api_listen: 127.0.0.1:9000\n"))
	if err != nil {
		t.Fatalf("a file with only settings should be accepted: %v", err)
	}
	if len(c.Targets) != 0 {
		t.Errorf("targets = %v, want none", c.Targets)
	}
	if c.Database != defaultDatabase {
		t.Errorf("defaults were not applied: database = %q", c.Database)
	}
}

// And so is no file at all.
func TestLoadOrDefaultsWithoutAFile(t *testing.T) {
	c, err := LoadOrDefaults(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("a missing file should fall back to defaults: %v", err)
	}
	if c.APIListen != defaultAPIListen || c.Retention.MaxAgeDuration() == 0 {
		t.Errorf("defaults were not applied: %+v", c)
	}

	// A file that exists but cannot be read is still an error: silently
	// starting with defaults would hide a typo in the path.
	dir := t.TempDir()
	if _, err := LoadOrDefaults(dir); err == nil {
		t.Error("a directory given as the config path should be reported")
	}
}

func TestProjectNameComesFromTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "core-delpagroup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectNameFor(filepath.Join(dir, "sonda.yaml")); got != "core-delpagroup" {
		t.Errorf("project name = %q, want the directory name", got)
	}
}

func TestGRPCTargetDefaults(t *testing.T) {
	c, err := Parse([]byte(`
targets:
  - {name: orders, listen: 127.0.0.1:9201, upstream: "http://127.0.0.1:8082", protocol: grpc}
`))
	if err != nil {
		t.Fatal(err)
	}
	// Reflection defaults to on: asking costs one call and fails harmlessly.
	if !c.Targets[0].ReflectionEnabled() {
		t.Error("reflection should default to enabled for grpc targets")
	}

	off, err := Parse([]byte(`
targets:
  - {name: orders, listen: 127.0.0.1:9201, upstream: "http://127.0.0.1:8082", protocol: grpc, reflection: false}
`))
	if err != nil {
		t.Fatal(err)
	}
	if off.Targets[0].ReflectionEnabled() {
		t.Error("an explicit reflection: false must turn it off")
	}
}

func TestValidationRejectsBadConfigs(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantMsg string
	}{
		{
			name: "duplicate name",
			yaml: `
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3000"}
  - {name: api, listen: 127.0.0.1:9102, upstream: "http://127.0.0.1:3001"}
`,
			wantMsg: "duplicate name",
		},
		{
			// The failure mode this catches is a target silently never
			// receiving traffic because another one took the port.
			name: "duplicate listen address",
			yaml: `
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3000"}
  - {name: billing, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3001"}
`,
			wantMsg: "already in use",
		},
		{
			name: "target collides with the api port",
			yaml: `
api_listen: 127.0.0.1:9000
targets:
  - {name: api, listen: 127.0.0.1:9000, upstream: "http://127.0.0.1:3000"}
`,
			wantMsg: "already in use",
		},
		{
			name: "upstream without scheme",
			yaml: `
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "127.0.0.1:3000"}
`,
			wantMsg: "must start with http",
		},
		{
			name: "unknown protocol",
			yaml: `
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3000", protocol: ftp}
`,
			wantMsg: "not supported",
		},
		{
			// Silently ignoring these on an http target would leave the user
			// waiting for decoding that is never going to happen.
			name: "schema options on a non-grpc target",
			yaml: `
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3000", reflection: true}
`,
			wantMsg: "only apply to",
		},
		{
			name: "descriptor set that does not exist",
			yaml: `
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:50051", protocol: grpc, descriptor_set: ./nope.binpb}
`,
			wantMsg: "descriptor_set",
		},
		{
			name: "bad duration",
			yaml: `
retention:
  max_age: "for a while"
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3000"}
`,
			wantMsg: "retention.max_age",
		},
		{
			name: "unknown key",
			yaml: `
databse: sonda.db
targets:
  - {name: api, listen: 127.0.0.1:9101, upstream: "http://127.0.0.1:3000"}
`,
			wantMsg: "databse",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}
