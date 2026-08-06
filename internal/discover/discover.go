// Package discover reads a project's own configuration to find the services it
// talks to.
//
// Setting up fifteen services by hand is the reason a tool like this gets
// abandoned after one afternoon. The addresses are already written down
// somewhere — a .env full of SERVICE_URL entries, a compose file with published
// ports — so the honest move is to read that instead of asking the developer to
// retype it.
//
// Nothing here guesses silently. Every service comes back with the line it was
// found on, so a wrong reading is visible before anything is saved.
package discover

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Found is one service read out of a file, with the evidence for it.
type Found struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	Protocol string `json:"protocol"`
	// Listen is a suggestion, not a decision: the same last two digits as the
	// real port, prefixed, so the correspondence reads itself.
	Listen string `json:"listen"`
	Source string `json:"source"`
}

// envLine matches the shape every one of these files uses: a name, an equals
// sign, and a host:port. Quotes and inline comments are common enough to handle
// rather than choke on.
var envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Z][A-Z0-9_]*)\s*=\s*["']?([^"'#\s]+)["']?`)

// hostPort pulls an address out of a value that may or may not carry a scheme.
var hostPort = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*://)?([A-Za-z0-9._-]+):(\d{2,5})(?:/.*)?$`)

// FromEnv reads a .env-style file.
func FromEnv(r io.Reader) ([]Found, error) {
	var out []Found
	seen := map[string]bool{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	line := 0

	for scanner.Scan() {
		line++
		text := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		match := envLine.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		key, value := match[1], match[2]

		addr := hostPort.FindStringSubmatch(value)
		if addr == nil {
			continue
		}
		host, port := addr[1], addr[2]

		// Anything that is not a service address: database URLs, secrets that
		// happen to contain a colon, the process's own port.
		if !looksLikeService(key) {
			continue
		}
		name := serviceName(key)
		if seen[name] {
			continue
		}
		seen[name] = true

		out = append(out, Found{
			Name:     name,
			Upstream: "http://" + host + ":" + port,
			Protocol: protocolFor(key),
			Listen:   suggestListen(port),
			Source:   fmt.Sprintf("línea %d: %s", line, key),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// looksLikeService keeps entries that name something to call and drops the rest.
// Being strict here matters more than being clever: a list with a database
// connection string in it is worse than a list missing one service.
func looksLikeService(key string) bool {
	if !strings.HasSuffix(key, "_URL") && !strings.HasSuffix(key, "_ADDR") &&
		!strings.HasSuffix(key, "_ADDRESS") && !strings.HasSuffix(key, "_HOST") {
		return false
	}
	for _, noise := range []string{
		"DATABASE", "POSTGRES", "MYSQL", "MONGO", "REDIS", "AMQP", "RABBIT",
		"KAFKA", "ELASTIC", "S3", "SMTP", "SENTRY", "OTEL", "JAEGER",
		"FRONTEND", "PUBLIC", "CALLBACK", "REDIRECT", "WEBHOOK",
	} {
		if strings.Contains(key, noise) {
			return false
		}
	}
	return true
}

// serviceName turns MS_OCEAN_TRACKING_GRPC_URL into ms-ocean-tracking.
func serviceName(key string) string {
	name := key
	for _, suffix := range []string{"_GRPC_URL", "_HTTP_URL", "_URL", "_ADDRESS", "_ADDR", "_HOST"} {
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

func protocolFor(key string) string {
	if strings.Contains(key, "GRPC") {
		return "grpc"
	}
	return "http"
}

// suggestListen keeps the last two digits of the real port so the mapping is
// readable at a glance: 50052 becomes 9152, 3000 becomes 9100.
func suggestListen(port string) string {
	n, err := strconv.Atoi(port)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", 9100+n%100)
}

// FromCompose reads published ports out of a compose file.
//
// Deliberately line-based rather than a YAML parse: the only thing needed is
// the service name and its published port, a compose file with anchors and
// extensions parses into a shape that still needs the same walking, and a
// misread here is visible in the preview before anything is saved.
func FromCompose(r io.Reader) ([]Found, error) {
	var (
		out     []Found
		service string
		indent  int
	)

	nameLine := regexp.MustCompile(`^(\s+)([a-zA-Z0-9._-]+):\s*$`)
	portLine := regexp.MustCompile(`["']?(?:[\d.]+:)?(\d{2,5}):(\d{2,5})["']?`)

	scanner := bufio.NewScanner(r)
	line, inServices, inPorts := 0, false, false

	for scanner.Scan() {
		line++
		text := scanner.Text()
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if trimmed == "services:" {
			inServices = true
			continue
		}
		if !inServices {
			continue
		}
		// A top-level key ends the services block.
		if !strings.HasPrefix(text, " ") && !strings.HasPrefix(text, "\t") {
			inServices = false
			continue
		}

		if match := nameLine.FindStringSubmatch(text); match != nil {
			leading := len(match[1])
			if service == "" || leading <= indent {
				service, indent, inPorts = match[2], leading, false
				continue
			}
			inPorts = match[2] == "ports"
			continue
		}
		if !inPorts || service == "" {
			continue
		}
		if match := portLine.FindStringSubmatch(trimmed); match != nil {
			published := match[1]
			out = append(out, Found{
				Name:     service,
				Upstream: "http://127.0.0.1:" + published,
				Protocol: "http",
				Listen:   suggestListen(published),
				Source:   fmt.Sprintf("línea %d: %s", line, service),
			})
			inPorts = false
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Detect picks a reader by what the filename looks like.
func Detect(filename string, r io.Reader) ([]Found, error) {
	lower := strings.ToLower(filename)
	switch {
	case strings.Contains(lower, "compose"):
		return FromCompose(r)
	default:
		return FromEnv(r)
	}
}

// Valid reports whether a discovered entry can be saved as it stands.
func (f Found) Valid() error {
	if f.Name == "" {
		return fmt.Errorf("no name")
	}
	u, err := url.Parse(f.Upstream)
	if err != nil || u.Host == "" {
		return fmt.Errorf("upstream %q is not an address", f.Upstream)
	}
	return nil
}
