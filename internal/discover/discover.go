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

	// Key and Original are the variable this came from and its value exactly as
	// written. Source already says both, but as prose meant for a person to
	// read. Making the caller parse "línea 12: MS_AUTH_GRPC_URL" back apart is
	// how you get a tool that breaks when the wording changes — and the whole
	// point of connecting a project is handing back the edit to make, which
	// needs the name and the old value verbatim.
	Key      string `json:"key,omitempty"`
	Original string `json:"original,omitempty"`
}

// envLine matches the shape every one of these files uses: a name, an equals
// sign, and a host:port. Quotes and inline comments are common enough to handle
// rather than choke on.
var envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Z][A-Z0-9_]*)\s*=\s*["']?([^"'#\s]+)["']?`)

// hostPort pulls an address out of a value that may or may not carry a scheme.
// The scheme is captured rather than discarded because https:// is the one
// piece of it that changes what Sonda has to do: rewriting a TLS upstream as
// http:// produces a service that is configured, listed, and unreachable.
var hostPort = regexp.MustCompile(`^(?:([a-z][a-z0-9+.-]*)://)?([A-Za-z0-9._-]+):(\d{2,5})(?:/.*)?$`)

// FromEnv reads a .env-style file.
func FromEnv(r io.Reader) ([]Found, error) {
	var out []Found
	seen := map[string]bool{}
	taken := map[string]bool{}

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
		scheme, host, port := addr[1], addr[2], addr[3]
		// Anything else — grpc://, tcp:// — is a transport Sonda speaks over
		// plaintext HTTP, and inventing https from an unknown scheme would be a
		// guess. Nothing here ever reads a key that could carry a credential,
		// and a scheme is not one.
		if scheme != "https" {
			scheme = "http"
		}

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
			Upstream: scheme + "://" + host + ":" + port,
			Protocol: protocolFor(key),
			Listen:   FreeListen(suggestListen(port), taken),
			Source:   fmt.Sprintf("línea %d: %s", line, key),
			Key:      key,
			Original: value,
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

// FreeListen resolves the collision the suggestion cannot avoid on its own.
//
// Only the last two digits survive, so 3001 and 50001 are both suggested
// 127.0.0.1:9101 — and two services told to listen on one address is not a
// clash anyone can see coming: neither port is bound when the suggestion is
// made, so probing says both are free, and after activation one listener binds,
// the other reports "address already in use", and which one wins is whichever
// was reconciled first. It reads exactly like an external process squatting the
// port.
//
// taken is what this pass has already handed out. It is the caller's, because
// one project is usually several files and a suggestion is only unique if the
// whole run agrees on it.
func FreeListen(addr string, taken map[string]bool) string {
	host, port, err := splitPort(addr)
	if err != nil {
		return addr
	}
	for ; port <= 65535; port++ {
		candidate := fmt.Sprintf("%s:%d", host, port)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
	return addr
}

func splitPort(addr string) (host string, port int, err error) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 {
		return "", 0, fmt.Errorf("%q is not host:port", addr)
	}
	port, err = strconv.Atoi(addr[i+1:])
	return addr[:i], port, err
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
	taken := map[string]bool{}

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
				Listen:   FreeListen(suggestListen(published), taken),
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
