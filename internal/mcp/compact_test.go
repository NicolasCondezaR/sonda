package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// diagnosisFixture is the shape the API answers with, in the two readings that
// matter: a fleet of quiet services whose sentences differ only by their own
// address, and one that is genuinely broken.
func diagnosisFixture() map[string]any {
	quiet := func(name, listen, env, upstream string) map[string]any {
		return map[string]any{
			"service": name, "listen": listen, "upstream": upstream,
			"protocol": "grpc", "expects": "h2c", "point_at": env + "=" + listen,
			"listening": true, "connections": float64(0), "captures": float64(0),
			"faults": float64(0), "upstream_probed": false,
			"verdict": "no_connections",
			"detail":  "Nothing has connected to this port since it opened. Sonda is listening and no client has arrived.",
			"cannot_distinguish": []any{
				"the caller is still pointed at the service itself instead of at " + listen,
				"the caller is pointed at a port that is not " + listen,
				"the caller has simply not made the call yet",
			},
			"what_to_check": []any{
				"Point the caller at Sonda: " + env + "=" + listen,
				"Restart whatever reads that setting.",
			},
		}
	}
	return map[string]any{
		"project": "core-delpagroup",
		"verdict": "no_connections",
		"summary": "nothing has connected",
		"note":    "Sonda cannot see a client that never connected.",
		"services": []any{
			quiet("ms-auth", "127.0.0.1:9152", "MS_AUTH_GRPC_URL", "localhost:50052"),
			quiet("ms-rates", "127.0.0.1:9151", "MS_RATES_GRPC_URL", "localhost:50051"),
			quiet("ms-tracking", "127.0.0.1:9153", "MS_TRACKING_GRPC_URL", "localhost:50053"),

			// Two services on the same verdict whose prose is genuinely
			// different, because the reading itself contains counts and those
			// are not fields that can be lifted. Grouping by verdict alone
			// would merge these and report one service's counts as the other's.
			// The fixture carries them so that mistake cannot pass.
			map[string]any{
				"service": "echo", "listen": "127.0.0.1:9101", "upstream": "http://localhost:8081",
				"protocol": "http", "expects": "plaintext HTTP", "point_at": "ECHO_URL=http://127.0.0.1:9101",
				"listening": true, "connections": float64(40), "captures": float64(12),
				"faults": float64(1), "upstream_probed": false,
				"verdict": "capturing",
				"detail":  "12 call(s) captured here, 1 flagged, the newest 5s ago. Traffic is reaching Sonda.",
				"what_to_check": []any{
					"There are captures, so the proxy is working.",
				},
			},
			map[string]any{
				"service": "orders", "listen": "127.0.0.1:9102", "upstream": "http://localhost:8082",
				"protocol": "http", "expects": "plaintext HTTP", "point_at": "ORDERS_URL=http://127.0.0.1:9102",
				"listening": true, "connections": float64(9), "captures": float64(3),
				"faults": float64(0), "upstream_probed": false,
				"verdict": "capturing",
				"detail":  "3 call(s) captured here, 0 flagged, the newest 2m ago. Traffic is reaching Sonda.",
				"what_to_check": []any{
					"There are captures, so the proxy is working.",
				},
			},

			map[string]any{
				"service": "gateway", "listen": "127.0.0.1:9180", "upstream": "http://localhost:8080",
				"protocol": "http", "expects": "plaintext HTTP", "point_at": "GATEWAY_URL=http://127.0.0.1:9180",
				"listening": false, "connections": float64(0), "captures": float64(0),
				"faults": float64(0), "upstream_probed": false,
				"listen_error": "address already in use",
				"verdict":      "listener_down",
				"detail":       "The port never opened, so nothing could have reached Sonda here: address already in use",
				"what_to_check": []any{
					"Something else is holding 127.0.0.1:9180. Free it, or move this service to another port.",
				},
			},
		},
	}
}

// rebuild turns a compacted report back into the per-service list the API sent,
// which is the only way to state "nothing was dropped" as a check rather than as
// a claim in a comment.
func rebuild(t *testing.T, compacted map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}

	groups, ok := compacted["groups"].([]any)
	if !ok {
		t.Fatalf("no groups in the compacted report: %v", compacted)
	}
	for _, item := range groups {
		g := item.(map[string]any)
		shared, _ := g["same_for_all"].(map[string]any)

		for _, m := range g["members"].([]any) {
			member := m.(map[string]any)
			svc := map[string]any{}

			for k, v := range shared {
				svc[k] = v
			}
			for k, v := range member {
				svc[k] = v
			}
			svc["verdict"] = g["verdict"]

			// Resolve the placeholders from this member's own fields, which is
			// exactly what the shape note tells a reader to do.
			resolve := func(s string) string {
				for _, name := range lifted {
					if v, ok := svc[name].(string); ok {
						s = strings.ReplaceAll(s, "{"+name+"}", v)
					}
				}
				return s
			}
			for _, key := range []string{"detail"} {
				if s, ok := g[key].(string); ok {
					svc[key] = resolve(s)
				}
			}
			for _, key := range []string{"cannot_distinguish", "what_to_check"} {
				if list, ok := g[key].([]any); ok {
					lines := make([]any, 0, len(list))
					for _, line := range list {
						lines = append(lines, resolve(line.(string)))
					}
					svc[key] = lines
				}
			}
			out[svc["service"].(string)] = svc
		}
	}
	return out
}

// The whole justification for compacting instead of summarising: the reader can
// get back exactly what the API said. If this fails, the tool is deciding for
// the agent which services matter.
func TestCompactingTheDiagnosisLosesNothing(t *testing.T) {
	original := diagnosisFixture()

	// Deep-copy through JSON, because compactDiagnosis edits the members it is
	// handed and the comparison has to be against what the API really sent.
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}

	got := rebuild(t, compactDiagnosis(original))

	for _, item := range before["services"].([]any) {
		want := item.(map[string]any)
		name := want["service"].(string)
		have, ok := got[name]
		if !ok {
			t.Errorf("%s vanished from the compacted report", name)
			continue
		}
		if !reflect.DeepEqual(want, have) {
			t.Errorf("%s did not survive the round trip:\n want %v\n  got %v", name, want, have)
		}
	}
	if len(got) != len(before["services"].([]any)) {
		t.Errorf("rebuilt %d services from a report of %d", len(got), len(before["services"].([]any)))
	}
}

// Two readings that are genuinely different must not be merged. Grouping is
// only allowed to fold repetition, never disagreement.
func TestAGenuinelyDifferentReadingIsNotMergedAway(t *testing.T) {
	compacted := compactDiagnosis(diagnosisFixture())
	groups := compacted["groups"].([]any)

	// Four: the three quiet services fold into one, the broken listener stands
	// alone, and the two capturing services stay apart because their readings
	// carry different counts.
	if len(groups) != 4 {
		t.Fatalf("%d groups, want 4 — folding a reading that differs would report one service's counts as another's", len(groups))
	}
	first := groups[0].(map[string]any)
	if first["verdict"] != "listener_down" {
		t.Errorf("first group is %q; the one naming a problem has to come first", first["verdict"])
	}
	if n := first["services"]; n != 1 {
		t.Errorf("the broken group holds %v services, want 1", n)
	}
}

// The three quiet services said the same thing about different addresses, and
// that is the case the whole file exists for.
func TestServicesThatReadTheSameAreStatedOnce(t *testing.T) {
	compacted := compactDiagnosis(diagnosisFixture())

	var quiet map[string]any
	for _, item := range compacted["groups"].([]any) {
		g := item.(map[string]any)
		if g["verdict"] == "no_connections" {
			quiet = g
		}
	}
	if quiet == nil {
		t.Fatal("the quiet services did not group")
	}
	if n := quiet["services"]; n != 3 {
		t.Fatalf("the quiet group holds %v services, want 3", n)
	}

	lines := quiet["cannot_distinguish"].([]any)
	if len(lines) != 3 {
		t.Fatalf("the shared reading has %d lines, want the 3 stated once", len(lines))
	}
	joined := ""
	for _, l := range lines {
		joined += l.(string) + " "
	}
	if !strings.Contains(joined, "{listen}") {
		t.Errorf("the shared lines kept a literal address instead of the placeholder: %q", joined)
	}
	for _, address := range []string{"9151", "9152", "9153"} {
		if strings.Contains(joined, address) {
			t.Errorf("the shared reading still names one member's own port %s: %q", address, joined)
		}
	}

	// Fields every member agreed on are stated once too.
	shared := quiet["same_for_all"].(map[string]any)
	for _, key := range []string{"connections", "captures", "listening"} {
		if _, ok := shared[key]; !ok {
			t.Errorf("%q was identical on all three members and was not lifted", key)
		}
	}
	// But never the two that tell the members apart.
	for _, key := range []string{"service", "listen"} {
		if _, ok := shared[key]; ok {
			t.Errorf("%q was lifted; the members can no longer be told apart", key)
		}
	}
}

// point_at contains listen. Substituting the short value first would leave
// "MS_AUTH_GRPC_URL={listen}" and a {point_at} that never matches, so the
// rebuilt sentence would differ from the original.
func TestTheLongerValueIsSubstitutedFirst(t *testing.T) {
	svc := map[string]any{
		"service": "ms-auth", "listen": "127.0.0.1:9152",
		"point_at": "MS_AUTH_GRPC_URL=127.0.0.1:9152",
		"verdict":  "no_connections",
		"what_to_check": []any{
			"Point the caller at Sonda: MS_AUTH_GRPC_URL=127.0.0.1:9152",
		},
	}
	prose, _ := splitDiagnosis(svc)
	got := prose["what_to_check"].([]any)[0].(string)

	if want := "Point the caller at Sonda: {point_at}"; got != want {
		t.Errorf("templated to %q, want %q", got, want)
	}
}

// A report of one service must not grow a same_for_all wrapper: there is nothing
// to share, and hoisting its fields would make the answer longer.
func TestASingleServiceIsNotWrappedInSharedFields(t *testing.T) {
	report := map[string]any{
		"verdict": "capturing",
		"services": []any{
			map[string]any{
				"service": "echo", "listen": "127.0.0.1:9101", "verdict": "capturing",
				"captures": float64(12), "detail": "12 call(s) captured here.",
			},
		},
	}
	g := compactDiagnosis(report)["groups"].([]any)[0].(map[string]any)

	if _, ok := g["same_for_all"]; ok {
		t.Error("a group of one lifted its fields, which only makes the answer longer")
	}
	member := g["members"].([]any)[0].(map[string]any)
	if member["captures"] != float64(12) {
		t.Errorf("the single member lost its reading: %v", member)
	}
}

// An answer with no services at all is handed back untouched. Wrapping an error
// or an empty report in a groups key would make every caller handle two shapes
// for no gain.
func TestAReportWithNoServicesIsUntouched(t *testing.T) {
	for _, body := range []map[string]any{
		{"verdict": "no_project", "summary": "no project is active"},
		{"verdict": "no_services", "services": []any{}},
	} {
		got := compactDiagnosis(body)
		if _, ok := got["groups"]; ok {
			t.Errorf("a report with no services grew groups: %v", got)
		}
	}
}
