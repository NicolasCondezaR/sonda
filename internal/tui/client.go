package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"strconv"
	"strings"
	"time"
)

// Client talks to the same HTTP API the web interface uses. Nothing here
// reaches into the database: the API is the contract, and keeping the terminal
// client on the far side of it is what made this package small.
type Client struct {
	base string
	http *http.Client
}

func NewClient(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

type Target struct {
	Name     string `json:"name"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	Protocol string `json:"protocol"`
}

type Call struct {
	ID             int64   `json:"id"`
	Target         string  `json:"target"`
	Protocol       string  `json:"protocol"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	Status         int     `json:"status"`
	StartedAt      string  `json:"started_at"`
	DurationMS     float64 `json:"duration_ms"`
	Error          string  `json:"error"`
	RequestSize    int64   `json:"request_size"`
	ResponseSize   int64   `json:"response_size"`
	GRPCStatus     *int32  `json:"grpc_status"`
	GRPCStatusText string  `json:"grpc_status_text"`
	GRPCMessage    string  `json:"grpc_message"`
	GraphQLOp      string  `json:"graphql_op"`
	GraphQLErrors  int     `json:"graphql_errors"`

	// A Postgres capture has no status, and its path is a database every one of
	// them shares, so the summary — the statement and how it ended — is the only
	// thing that says which one this was, and its errors are the only thing that
	// says it failed.
	PostgresSummary string `json:"postgres_summary"`
	PostgresErrors  int    `json:"postgres_errors"`
	ReplayOf        *int64 `json:"replay_of"`
	StubOf          *int64 `json:"stub_of"`
	Injected        bool   `json:"injected"`
	TraceID         string `json:"trace_id"`

	started time.Time // parsed once, on the way in
}

// Started is the parse of StartedAt, done when the call is received so the
// render loop never parses a timestamp.
func (c Call) Started() time.Time { return c.started }

// Fault applies the same definition the server does, so the terminal and the
// web client can never disagree about what counts as a failure. It mirrors
// summaryFailed in internal/api, clause for clause and in the same order.
func (c Call) Fault() bool {
	if c.Error != "" {
		return true
	}
	// A GraphQL error arrives under HTTP 200 with no transport complaint, so it
	// has to be asked about before the status is trusted — and a Postgres
	// session has no status at all.
	if c.GraphQLErrors > 0 || c.PostgresErrors > 0 {
		return true
	}
	if c.GRPCStatus != nil {
		return *c.GRPCStatus != 0
	}
	return c.Status >= 400
}

// Outcome is the short form for a one-line listing.
func (c Call) Outcome() string {
	switch {
	case c.Error != "":
		return "TRANSPORT"
	case c.GRPCStatusText != "":
		return strings.ToUpper(c.GRPCStatusText)
	case c.GraphQLErrors > 0:
		// "200" here would be the truth about the transport and a lie about
		// the call.
		return "GRAPHQL ERROR"
	case c.PostgresErrors > 0:
		return "SQL ERROR"
	case c.Protocol == "postgres":
		// There is no status. Printing the zero would invent one, so the kind
		// of row stands in: a statement, or the connection that ran none.
		return c.Method
	default:
		return strconv.Itoa(c.Status)
	}
}

// Label is how a call is named in one line. For GraphQL and Postgres alike the
// method and path are the same on every call a service makes, and the
// operation — or the statement — is the only part that says which one this was.
// A Postgres row is one statement, so its summary is the SQL itself.
func (c Call) Label() string {
	base := c.Method + " " + c.Path
	switch {
	case c.GraphQLOp != "":
		return base + " · " + c.GraphQLOp
	case c.PostgresSummary != "":
		return base + " · " + c.PostgresSummary
	}
	return base
}

type Message struct {
	Headers   map[string][]string `json:"headers"`
	Text      string              `json:"text"`
	Base64    string              `json:"base64"`
	Size      int64               `json:"size"`
	Stored    int                 `json:"stored"`
	Truncated bool                `json:"truncated"`
}

type GRPCMessage struct {
	Index      int             `json:"index"`
	Size       int             `json:"size"`
	Compressed bool            `json:"compressed"`
	JSON       json.RawMessage `json:"json"`
	Error      string          `json:"error"`
}

type GRPCView struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Schema  struct {
		Source string `json:"source"`
		Error  string `json:"error"`
	} `json:"schema"`
	Request  []GRPCMessage `json:"request"`
	Response []GRPCMessage `json:"response"`
}

type CallDetail struct {
	Call
	ClientAddr       string              `json:"client_addr"`
	Request          Message             `json:"request"`
	Response         Message             `json:"response"`
	ResponseTrailers map[string][]string `json:"response_trailers"`
	GRPC             *GRPCView           `json:"grpc"`

	// A socket and an event stream are the same shape as a gRPC stream: one
	// exchange that carried many messages, decoded by the API. GraphQL is the
	// same arrangement over a single POST.
	Socket   *SocketView   `json:"socket"`
	Stream   *StreamView   `json:"stream"`
	GraphQL  *GraphQLView  `json:"graphql"`
	Postgres *PostgresView `json:"postgres"`
}

// PostgresView is one session read back as the protocol messages of both
// directions. The fields mirror pgwire.Message and carry only what the terminal
// draws; the API's own view has the rest.
type PostgresView struct {
	Sent               []PGMessage `json:"sent"`
	Received           []PGMessage `json:"received"`
	SentIncomplete     bool        `json:"sent_incomplete"`
	ReceivedIncomplete bool        `json:"received_incomplete"`
}

type PGMessage struct {
	Kind       string            `json:"kind"`
	Size       int64             `json:"size"`
	SQL        string            `json:"sql"`
	Statement  string            `json:"statement"`
	Params     []PGValue         `json:"params"`
	Tag        string            `json:"tag"`
	Severity   string            `json:"severity"`
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	Detail     string            `json:"detail"`
	Hint       string            `json:"hint"`
	TxStatus   string            `json:"tx_status"`
	Auth       string            `json:"auth"`
	Parameters map[string]string `json:"parameters"`
	Encrypted  bool              `json:"encrypted"`
	Note       string            `json:"note"`
}

type PGValue struct {
	Null   bool   `json:"null"`
	Size   int    `json:"size"`
	Binary bool   `json:"binary"`
	Text   string `json:"text"`
}

type GraphQLView struct {
	Batch      bool               `json:"batch"`
	Operations []GraphQLOperation `json:"operations"`
	Errors     int                `json:"errors"`
	Unreadable bool               `json:"unreadable"`
}

type GraphQLOperation struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Fields    []string        `json:"fields"`
	Variables json.RawMessage `json:"variables"`
	Errors    []GraphQLError  `json:"errors"`
}

type GraphQLError struct {
	Message string `json:"message"`
	Path    string `json:"path"`
	Code    string `json:"code"`
}

type SocketView struct {
	Sent            []FrameView `json:"sent"`
	Received        []FrameView `json:"received"`
	SentSummary     string      `json:"sent_summary"`
	ReceivedSummary string      `json:"received_summary"`
}

type FrameView struct {
	Kind        string `json:"kind"`
	Size        int64  `json:"size"`
	Text        string `json:"text"`
	CloseCode   int    `json:"close_code"`
	CloseReason string `json:"close_reason"`
}

type StreamView struct {
	Events     []EventView `json:"events"`
	Incomplete bool        `json:"incomplete"`
}

type EventView struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Data string `json:"data"`
}

type TargetStat struct {
	Target string `json:"target"`
	Calls  int64  `json:"calls"`
	Faults int64  `json:"faults"`
}

type Stats struct {
	Calls    int64        `json:"calls"`
	Dropped  int64        `json:"dropped"`
	ByTarget []TargetStat `json:"by_target"`
}

type Change struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	A    any    `json:"a"`
	B    any    `json:"b"`
}

type SideDiff struct {
	Comparable bool     `json:"comparable"`
	Reason     string   `json:"reason"`
	Identical  bool     `json:"identical"`
	Changes    []Change `json:"changes"`
	Messages   []struct {
		Index      int      `json:"index"`
		OnlyIn     string   `json:"only_in"`
		Comparable bool     `json:"comparable"`
		Reason     string   `json:"reason"`
		Identical  bool     `json:"identical"`
		Changes    []Change `json:"changes"`
	} `json:"messages"`
}

type Diff struct {
	Metadata []Change `json:"metadata"`
	Request  SideDiff `json:"request"`
	Response SideDiff `json:"response"`
}

type ReplayResult struct {
	SentTo     string  `json:"sent_to"`
	Status     int     `json:"status"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error"`
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiError keeps the server's own explanation. The refusal to replay a
// truncated capture says how to fix it, and swallowing that in favour of a
// status code would throw away the useful half.
func apiError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return fmt.Errorf("%s", body.Error)
	}
	return fmt.Errorf("the API answered %d", resp.StatusCode)
}

func (c *Client) Targets(ctx context.Context) ([]Target, error) {
	var out struct {
		Targets []Target `json:"targets"`
	}
	err := c.get(ctx, "/api/targets", &out)
	return out.Targets, err
}

func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	err := c.get(ctx, "/api/stats", &out)
	return out, err
}

// Calls lists captures. The window is applied server-side so the terminal never
// holds more than it draws.
func (c *Client) Calls(ctx context.Context, failedOnly bool, search string, window time.Duration, limit int) ([]Call, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("since", time.Now().Add(-window).UTC().Format(time.RFC3339Nano))
	if failedOnly {
		q.Set("failed", "true")
	}
	if search != "" {
		q.Set("q", search)
	}

	var out struct {
		Calls []Call `json:"calls"`
	}
	if err := c.get(ctx, "/api/calls?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	for i := range out.Calls {
		out.Calls[i].parseTime()
	}
	return out.Calls, nil
}

func (c *Call) parseTime() {
	if t, err := time.Parse(time.RFC3339Nano, c.StartedAt); err == nil {
		c.started = t
	}
}

func (c *Client) Detail(ctx context.Context, id int64) (*CallDetail, error) {
	var out CallDetail
	if err := c.get(ctx, "/api/calls/"+strconv.FormatInt(id, 10), &out); err != nil {
		return nil, err
	}
	out.parseTime()
	return &out, nil
}

func (c *Client) Diff(ctx context.Context, a, b int64) (*Diff, error) {
	var out Diff
	path := fmt.Sprintf("/api/diff?a=%d&b=%d", a, b)
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Trace asks for the whole request a call belonged to.
//
// The API already returns the tree drawn as text, and the terminal takes it as
// it comes: a second renderer here would be a second thing to keep in step with
// the first, for a drawing that is identical either way.
func (c *Client) Trace(ctx context.Context, id int64) (*Trace, error) {
	var out Trace
	if err := c.get(ctx, "/api/trace?call="+strconv.FormatInt(id, 10), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Trace struct {
	Rendered string `json:"rendered"`
	Tree     struct {
		Calls   int  `json:"calls"`
		Failed  int  `json:"failed"`
		Certain bool `json:"certain"`
	} `json:"trace"`
}

// Drift asks whether an endpoint still answers the shape it used to.
func (c *Client) Drift(ctx context.Context, call Call) (*Drift, error) {
	q := url.Values{}
	q.Set("target", call.Target)
	q.Set("path", call.Path)
	q.Set("method", call.Method)

	var out Drift
	if err := c.get(ctx, "/api/drift?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Drift struct {
	Baseline int64         `json:"baseline"`
	Rendered string        `json:"rendered"`
	Breaking []DriftChange `json:"breaking"`
	Changes  []DriftChange `json:"changes"`
}

type DriftChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (c *Client) Replay(ctx context.Context, id int64) (*ReplayResult, error) {
	url := fmt.Sprintf("%s/api/calls/%d/replay", c.base, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out ReplayResult
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

// Stream follows the live feed and pushes each call onto the channel. It
// reconnects on its own: the point of the terminal client is to be left open,
// and a restarted Sonda should not leave it silently dead.
func (c *Client) Stream(ctx context.Context, out chan<- Call) {
	for ctx.Err() == nil {
		c.streamOnce(ctx, out)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, out chan<- Call) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/stream", nil)
	if err != nil {
		return
	}
	// The stream has no deadline; the shared client's timeout would cut it.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var call Call
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &call) != nil {
			continue
		}
		call.parseTime()
		select {
		case out <- call:
		case <-ctx.Done():
			return
		}
	}
}
