package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/store"
	"github.com/NicolasCondezaR/sonda/internal/supervisor"
)

// "I pointed it at my service and I see nothing" is the first thing a tool like
// this gets wrong, and every cause of it looks identical from outside: a port
// that never opened, a caller still talking to the service directly, a client
// speaking TLS to a plaintext listener, a protocol Sonda does not listen for,
// or nothing having happened yet.
//
// Sonda already holds nearly all of the evidence — whether the listener bound,
// how many connections the port accepted, what was captured and when. This
// gathers it into one answer, and where the evidence genuinely does not
// separate two causes it says so and says what would. A confident wrong
// diagnosis is worse than none, and that rule bites hardest here: the reader is
// already confused, so an invented cause sends them down a road with nothing at
// the end of it.

// Verdicts, from the most certain reading to the least. They are stable strings
// because an agent branches on them; the sentences beside them are for people.
const (
	// verdictNoProject and verdictNoServices are whole-report verdicts: there is
	// nothing to observe, which is a configuration answer rather than a
	// per-service one.
	verdictNoProject  = "no_project"
	verdictNoServices = "no_services"

	// verdictListenerDown is the only cause Sonda knows with certainty and
	// alone: the socket never opened, so nothing could ever have arrived.
	verdictListenerDown = "listener_down"

	// verdictCapturing means traffic is being recorded for this service. An
	// empty screen with this verdict is the filter, the window or the channel.
	verdictCapturing = "capturing"

	// verdictConnectedNotCaptured is the reading that a bare capture count
	// cannot produce: something reached the port and never became a call.
	verdictConnectedNotCaptured = "connected_not_captured"

	// verdictUpstreamUnreachable is only ever reached after an explicit probe.
	verdictUpstreamUnreachable = "upstream_unreachable"

	// verdictNoConnections is the honest gap. Nothing has touched this port, and
	// Sonda cannot see a client that never connected to it.
	verdictNoConnections = "no_connections"
)

// severity orders the verdicts so the report can carry one overall reading
// without inventing an aggregate that means nothing. Highest wins.
var severity = map[string]int{
	verdictCapturing:            0,
	verdictNoConnections:        1,
	verdictUpstreamUnreachable:  2,
	verdictConnectedNotCaptured: 3,
	verdictListenerDown:         4,
	verdictNoServices:           5,
	verdictNoProject:            6,
}

// blindSpot is stated on every report, including the healthy ones. It is the
// one thing this feature cannot do, and a diagnosis that quietly omits its own
// limit is how a reader ends up trusting a reading it never made.
const blindSpot = "Sonda cannot see a client that never connected to it. A port with no connections reads " +
	"the same way whether the caller is pointed at the service directly, is pointed at the wrong port, or " +
	"simply has not run yet. The connection count is the only honest signal here, and it counts what " +
	"arrived, not what was meant to arrive."

type diagnosisJSON struct {
	Service  string `json:"service"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	Protocol string `json:"protocol"`

	// Expects is what this listener answers, in words. Half the causes of an
	// empty screen are a client and a listener disagreeing about it.
	Expects string `json:"expects"`

	// PointAt is the same line the projects view hands over: the one step no
	// amount of configuration removes.
	PointAt string `json:"point_at"`

	Listening   bool   `json:"listening"`
	ListenError string `json:"listen_error,omitempty"`

	Connections int64 `json:"connections"`
	Captures    int64 `json:"captures"`
	Faults      int64 `json:"faults"`

	LastCapture      string `json:"last_capture,omitempty"`
	LastCaptureAgeMS int64  `json:"last_capture_age_ms,omitempty"`

	UpstreamProbed    bool   `json:"upstream_probed"`
	UpstreamReachable bool   `json:"upstream_reachable,omitempty"`
	UpstreamError     string `json:"upstream_error,omitempty"`

	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`

	// CannotDistinguish are the causes still standing after the evidence ran
	// out, and WhatToCheck is what to do about them, in the order to do it.
	CannotDistinguish []string `json:"cannot_distinguish,omitempty"`
	WhatToCheck       []string `json:"what_to_check,omitempty"`
}

type diagnoseJSON struct {
	Project  string          `json:"project,omitempty"`
	Verdict  string          `json:"verdict"`
	Summary  string          `json:"summary"`
	Probed   bool            `json:"upstreams_probed"`
	Note     string          `json:"note"`
	Services []diagnosisJSON `json:"services"`
}

// diagnose answers "why do I see nothing".
//
// GET reads only what Sonda already knows and touches no network. POST does the
// same and additionally dials each upstream once. The split is the whole point:
// a probe is traffic the user did not send, so it can never be what a page
// refresh, a polling client or a timer does by accident. Both verbs return the
// same shape, so a caller that cannot probe still gets every other reading.
func (s *Server) diagnose(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildDiagnosis(r.Context(), r.Method == http.MethodPost))
}

func (s *Server) buildDiagnosis(ctx context.Context, probe bool) diagnoseJSON {
	out := diagnoseJSON{Note: blindSpot, Probed: probe, Services: []diagnosisJSON{}}

	active := s.rt.Active()
	if active == nil {
		out.Verdict = verdictNoProject
		out.Summary = "No project is active, so no port is open and nothing can be captured. " +
			"Activate one in PROJECTS, or from an agent with activate_project."
		return out
	}
	out.Project = active.Name

	if len(active.Services) == 0 {
		out.Verdict = verdictNoServices
		out.Summary = fmt.Sprintf("Project %q is active but has no services, so there is nothing listening. "+
			"Add one, or let Sonda read them out of the project's own .env or compose file.", active.Name)
		return out
	}

	// Stats are already scoped to the active project, which is what makes
	// "nothing captured" mean nothing captured here rather than nothing at all.
	stats, err := s.store.Stats(ctx, s.projectFilter())
	if err != nil {
		// A stats failure would silently turn every service into "no captures",
		// which is a confident wrong diagnosis of exactly the kind this feature
		// exists to refuse.
		out.Verdict = verdictNoProject
		out.Summary = "Sonda could not read its own capture counts, so it cannot tell you anything reliable: " + err.Error()
		return out
	}
	byTarget := make(map[string]store.TargetStats, len(stats.ByTarget))
	for _, t := range stats.ByTarget {
		byTarget[t.Target] = t
	}
	listeners := statusByKey(s.rt.Status())

	probes := map[int64]error{}
	if probe {
		probes = probeUpstreams(ctx, active.Services)
	}

	now := time.Now().UTC()
	worst := ""
	capturing := 0
	for _, svc := range active.Services {
		d := diagnose(svc, listeners[fmt.Sprintf("svc-%d", svc.ID)], byTarget[svc.Name], now)
		if probe {
			d.UpstreamProbed = true
			if err := probes[svc.ID]; err != nil {
				d.UpstreamError = err.Error()
			} else {
				d.UpstreamReachable = true
			}
			applyProbe(&d)
		}
		if d.Verdict == verdictCapturing {
			capturing++
		}
		if worst == "" || severity[d.Verdict] > severity[worst] {
			worst = d.Verdict
		}
		out.Services = append(out.Services, d)
	}

	out.Verdict = worst
	out.Summary = summarize(worst, capturing, len(out.Services), probe)
	return out
}

// diagnose reads one service. Every branch is ordered by how much the evidence
// actually settles, so a reading that rules something out comes before one that
// only narrows it.
func diagnose(svc store.Service, listener supervisor.Status, stats store.TargetStats, now time.Time) diagnosisJSON {
	d := diagnosisJSON{
		Service:     svc.Name,
		Listen:      svc.Listen,
		Upstream:    svc.Upstream,
		Protocol:    svc.Protocol,
		Expects:     expects(svc),
		PointAt:     pointAt(svc),
		Listening:   listener.Running,
		ListenError: listener.Error,
		Connections: listener.Connections,
		Captures:    stats.Calls,
		Faults:      stats.Faults,
	}
	if !stats.Last.IsZero() {
		d.LastCapture = stats.Last.Format(timeLayout)
		d.LastCaptureAgeMS = now.Sub(stats.Last).Milliseconds()
	}

	switch {
	case !d.Listening:
		// The only cause Sonda knows on its own, and it invalidates every other
		// reading for this service: nothing can arrive at a socket that is not
		// there.
		d.Verdict = verdictListenerDown
		d.Detail = fmt.Sprintf("The port never opened, so nothing could have reached Sonda here: %s",
			orElse(listener.Error, "the supervisor is not running this listener"))
		d.WhatToCheck = []string{
			fmt.Sprintf("Something else is holding %s. Free it, or move this service to another port.", svc.Listen),
			"Every other reading for this service is meaningless until the port opens.",
		}

	case d.Captures > 0:
		d.Verdict = verdictCapturing
		d.Detail = fmt.Sprintf("%d call(s) captured here, %d flagged, the newest %s ago. Traffic is reaching Sonda.",
			d.Captures, d.Faults, elapsed(d.LastCaptureAgeMS))
		d.WhatToCheck = []string{
			"There are captures, so the proxy is working. An empty field is the filter, the time window or the selected channel.",
			"Switch the filter to ALL and widen the window before looking anywhere else.",
		}

	case d.Connections > 0:
		// The reading a capture count alone cannot produce. Something found the
		// port; the bytes never became a call.
		d.Verdict = verdictConnectedNotCaptured
		d.Detail = fmt.Sprintf("%d connection(s) reached this port and none of them became a call. "+
			"Something is talking to Sonda here and Sonda is not understanding it.", d.Connections)
		d.CannotDistinguish = append(mismatch(svc),
			"the client speaks a protocol this listener does not — Sonda proxies http, grpc, postgres and amqp and nothing else, "+
				"so a Kafka, Redis or plain TCP client is accepted here and never understood",
			"the connection was opened and closed without a request, which is what a port scan or a dial-only health check looks like")
		d.WhatToCheck = []string{
			"This listener answers " + d.Expects + ".",
			"Point the caller at the address with its scheme: " + d.PointAt,
			"Read Sonda's own log. A refused TLS handshake is reported there and nowhere else, because it fails before a call exists.",
		}

	default:
		d.Verdict = verdictNoConnections
		d.Detail = "Nothing has connected to this port since it opened. Sonda is listening and no client has arrived."
		d.CannotDistinguish = []string{
			"the caller is still pointed at the service itself instead of at " + svc.Listen,
			"the caller is pointed at a port that is not " + svc.Listen,
			"the caller has simply not made the call yet",
		}
		d.WhatToCheck = []string{
			"Point the caller at Sonda: " + d.PointAt,
			"Restart whatever reads that setting. A process started before the change still holds the old address.",
			"Trigger the call and watch the connection count on this service. It counts connections, not calls, " +
				"so it moves even when the request itself is wrong — if it stays at zero, nothing is reaching Sonda.",
		}
	}
	return d
}

// applyProbe folds a dial result into a reading that was made without one.
//
// It can only ever add to the diagnosis, never overturn a stronger one: a
// listener that never opened and a service already capturing are settled
// regardless of what the upstream answers.
func applyProbe(d *diagnosisJSON) {
	if d.UpstreamReachable {
		if d.Verdict == verdictNoConnections {
			// Both facts are worth stating together, and neither of them
			// identifies the cause on its own.
			d.Detail += " The upstream accepts connections, so the service itself is up: if the caller is working at all, it is not going through Sonda."
		}
		return
	}
	if d.Verdict == verdictNoConnections {
		d.Verdict = verdictUpstreamUnreachable
		d.Detail = fmt.Sprintf("The upstream %s refused a connection (%s), and nothing has reached Sonda's port either. "+
			"The service is down, which has to be fixed before pointing anything at Sonda is worth doing.",
			d.Upstream, d.UpstreamError)
		d.WhatToCheck = append([]string{"Start the service at " + d.Upstream + " first."}, d.WhatToCheck...)
		return
	}
	// Any other verdict keeps its own reading; the dial is reported beside it.
	d.Detail += fmt.Sprintf(" The upstream %s also refused a connection (%s).", d.Upstream, d.UpstreamError)
}

// mismatch names the encryption mistake in the direction this listener would
// actually suffer it. Offering both directions every time would be padding: a
// plaintext listener cannot be the one demanding a handshake.
func mismatch(svc store.Service) []string {
	if svc.TLS {
		return []string{"the client is speaking in the clear to a listener that answers a TLS handshake"}
	}
	if svc.Protocol == config.ProtocolPostgres {
		// A Postgres client asks for encryption inside the protocol, so the
		// failure looks different and the fix is the client's sslmode.
		return []string{"the client demanded SSL and gave up when the connection was not upgraded — Sonda forwards the negotiation rather than terminating it, so a client set to sslmode=require against a plaintext upstream stops here"}
	}
	return []string{"the client is speaking TLS to a listener that answers in the clear"}
}

// expects says what the listener answers, because that is one half of a
// mismatch and the half the user cannot see from their own configuration.
func expects(svc store.Service) string {
	switch {
	case svc.Protocol == config.ProtocolPostgres:
		return "the PostgreSQL wire protocol, framed from the first byte, with no TLS in front of it"
	case svc.Protocol == config.ProtocolAMQP && svc.TLS:
		return "AMQP 0-9-1 over TLS, using a certificate Sonda mints itself"
	case svc.Protocol == config.ProtocolAMQP:
		return "the AMQP 0-9-1 wire protocol in plaintext, beginning with its AMQP protocol header"
	case svc.TLS && svc.Protocol == config.ProtocolGRPC:
		return "gRPC over HTTP/2 with TLS, using a certificate Sonda mints itself"
	case svc.TLS:
		return "HTTPS, using a certificate Sonda mints itself"
	case svc.Protocol == config.ProtocolGRPC:
		return "gRPC over cleartext HTTP/2 (h2c)"
	default:
		return "plaintext HTTP"
	}
}

func summarize(worst string, capturing, total int, probed bool) string {
	tail := ""
	if !probed {
		tail = " Upstreams were not probed: that dials the services and is only done when asked for."
	}

	switch worst {
	case verdictCapturing:
		return fmt.Sprintf("All %d service(s) are capturing. If the field is empty it is the filter, "+
			"the time window or the selected channel, not the proxy.%s", total, tail)
	case verdictListenerDown:
		return fmt.Sprintf("At least one port never opened, so that service can capture nothing at all. "+
			"%d of %d service(s) are capturing.%s", capturing, total, tail)
	case verdictConnectedNotCaptured:
		return fmt.Sprintf("Something reached a Sonda port and never became a call, which usually means the client "+
			"and the listener disagree about the protocol or about TLS. %d of %d service(s) are capturing.%s",
			capturing, total, tail)
	case verdictUpstreamUnreachable:
		return fmt.Sprintf("An upstream refused a connection and nothing has reached Sonda's port for it either. "+
			"%d of %d service(s) are capturing.%s", capturing, total, tail)
	default:
		return fmt.Sprintf("Nothing has connected to at least one Sonda port. Sonda cannot tell a caller that is "+
			"still bypassing it from one that has not run yet — see each service for what separates them. "+
			"%d of %d service(s) are capturing.%s", capturing, total, tail)
	}
}

// probeUpstreams dials every upstream once, concurrently.
//
// The dial goes straight to the service and never through Sonda's own listener
// — the same reason gRPC reflection does — so a probe can never land in the
// capture list looking like a call the user made. It also sends no bytes: an
// open TCP connection is the most a health check can honestly claim to have
// established, and anything more would be Sonda putting a request the user
// never wrote onto their service.
func probeUpstreams(ctx context.Context, services []store.Service) map[int64]error {
	// Bounded well under any sane client timeout: this runs while somebody
	// stares at an empty screen, and fifteen dead services must not add up to a
	// minute of waiting.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[int64]error, len(services))
	)
	for _, svc := range services {
		wg.Add(1)
		go func(svc store.Service) {
			defer wg.Done()
			err := probeUpstream(ctx, svc.Upstream)
			mu.Lock()
			out[svc.ID] = err
			mu.Unlock()
		}(svc)
	}
	wg.Wait()
	return out
}

func probeUpstream(ctx context.Context, upstream string) error {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not an address that can be dialled", upstream)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), defaultPort(u.Scheme))
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	return conn.Close()
}

func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "postgres", "postgresql":
		return "5432"
	case "amqps":
		return "5671"
	case "amqp":
		return "5672"
	default:
		return "80"
	}
}

// elapsed renders an age without rounding it into a lie. Under a minute it is
// the measurement; above it, seconds stay attached to the reading.
func elapsed(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	seconds := ms / 1000
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
}

func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
