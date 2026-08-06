package discover

import (
	"strconv"
	"strings"
	"testing"
)

func names(found []Found) []string {
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.Name)
	}
	return out
}

func TestFromEnvReadsServiceAddresses(t *testing.T) {
	env := `
# gRPC services
MS_AUTH_GRPC_URL=localhost:50052
MS_OCEAN_TRACKING_GRPC_URL=localhost:50061
export ADMIN_HTTP_URL="http://127.0.0.1:3000"
`
	found, err := FromEnv(strings.NewReader(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("found %v, want three services", names(found))
	}

	byName := map[string]Found{}
	for _, f := range found {
		byName[f.Name] = f
	}

	auth := byName["ms-auth"]
	if auth.Upstream != "http://localhost:50052" || auth.Protocol != "grpc" {
		t.Errorf("ms-auth = %+v", auth)
	}
	// The suggestion keeps the last two digits so the mapping reads itself.
	if auth.Listen != "127.0.0.1:9152" {
		t.Errorf("suggested %q for port 50052, want 127.0.0.1:9152", auth.Listen)
	}
	if byName["ms-ocean-tracking"].Name == "" {
		t.Error("a multi-word name did not become ms-ocean-tracking")
	}
	if byName["admin"].Protocol != "http" {
		t.Error("an entry without GRPC in its name should be http")
	}
}

// A list with a database connection string in it is worse than a list missing
// a service: the first gets saved and proxied, the second gets noticed.
func TestFromEnvIgnoresWhatIsNotAService(t *testing.T) {
	env := `
DATABASE_URL=postgres://user:pass@localhost:5432/db
REDIS_URL=redis://localhost:6379
POSTGRES_HOST=localhost:5433
KAFKA_ADDR=localhost:9092
SENTRY_URL=https://sentry.io:443
FRONTEND_URL=http://localhost:3001
NEXT_PUBLIC_API_URL=http://localhost:3000
OAUTH_CALLBACK_URL=http://localhost:3000/callback
JWT_SECRET=abc:def
PORT=8080
MS_AUTH_GRPC_URL=localhost:50052
`
	found, err := FromEnv(strings.NewReader(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "ms-auth" {
		t.Errorf("found %v, want only ms-auth", names(found))
	}
}

// Every entry carries the line it came from, so a wrong reading is visible
// before anything is saved.
func TestEveryEntryCarriesItsEvidence(t *testing.T) {
	found, err := FromEnv(strings.NewReader("\n\nMS_AUTH_GRPC_URL=localhost:50052\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatal("expected one service")
	}
	if !strings.Contains(found[0].Source, "3") || !strings.Contains(found[0].Source, "MS_AUTH_GRPC_URL") {
		t.Errorf("source = %q, want the line number and the key", found[0].Source)
	}
}

func TestFromEnvHandlesQuotesAndComments(t *testing.T) {
	env := `
MS_A_GRPC_URL='localhost:50051'   # the rates service
MS_B_GRPC_URL="localhost:50052"
MS_C_GRPC_URL=grpc://localhost:50053
`
	found, err := FromEnv(strings.NewReader(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Fatalf("found %v, want three", names(found))
	}
	for _, f := range found {
		if strings.ContainsAny(f.Upstream, `"'#`) {
			t.Errorf("quotes or a comment leaked into %q", f.Upstream)
		}
	}
}

func TestDuplicateKeysAreCollapsed(t *testing.T) {
	env := "MS_AUTH_GRPC_URL=localhost:50052\nMS_AUTH_GRPC_URL=localhost:50099\n"
	found, _ := FromEnv(strings.NewReader(env))
	if len(found) != 1 {
		t.Errorf("found %v, want one entry", names(found))
	}
}

func TestFromComposeReadsPublishedPorts(t *testing.T) {
	compose := `
services:
  api:
    image: api:latest
    ports:
      - "127.0.0.1:8081:8080"
  worker:
    image: worker:latest
  admin:
    image: admin:latest
    ports:
      - "3009:3000"

volumes:
  data:
`
	found, err := FromCompose(strings.NewReader(compose))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("found %v, want api and admin", names(found))
	}

	byName := map[string]Found{}
	for _, f := range found {
		byName[f.Name] = f
	}
	if byName["api"].Upstream != "http://127.0.0.1:8081" {
		t.Errorf("api = %q, want the published port", byName["api"].Upstream)
	}
	if byName["admin"].Upstream != "http://127.0.0.1:3009" {
		t.Errorf("admin = %q", byName["admin"].Upstream)
	}
	// A service with no published port cannot be reached, so it is not offered.
	if _, ok := byName["worker"]; ok {
		t.Error("a service with no published port was offered anyway")
	}
}

func TestDetectPicksTheReader(t *testing.T) {
	compose := "services:\n  api:\n    ports:\n      - \"8081:8080\"\n"
	found, err := Detect("docker-compose.yml", strings.NewReader(compose))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "api" {
		t.Errorf("compose was not read as compose: %v", names(found))
	}

	found, err = Detect(".env", strings.NewReader("MS_AUTH_GRPC_URL=localhost:50052\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "ms-auth" {
		t.Errorf("an env file was not read as env: %v", names(found))
	}
}

func TestSuggestedPortsDoNotCollideForCommonLayouts(t *testing.T) {
	// The real Delpa range: twenty-one consecutive ports.
	var env strings.Builder
	for port := 50051; port <= 50071; port++ {
		env.WriteString("MS_S")
		env.WriteString(strings.Repeat("X", port-50050))
		env.WriteString("_GRPC_URL=localhost:")
		env.WriteString(strconv.Itoa(port))
		env.WriteString("\n")
	}

	found, err := FromEnv(strings.NewReader(env.String()))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, f := range found {
		if other, clash := seen[f.Listen]; clash {
			t.Errorf("%s and %s were both suggested %s", other, f.Name, f.Listen)
		}
		seen[f.Listen] = f.Name
	}
}
