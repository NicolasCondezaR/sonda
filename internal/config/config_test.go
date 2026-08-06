package config

import (
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
			name:    "no targets",
			yaml:    "api_listen: 127.0.0.1:9000\n",
			wantMsg: "at least one target",
		},
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
databse: mirador.db
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
