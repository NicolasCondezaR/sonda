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
	ReplayOf       *int64  `json:"replay_of"`
	StubOf         *int64  `json:"stub_of"`
	TraceID        string  `json:"trace_id"`

	started time.Time // parsed once, on the way in
}

// Started is the parse of StartedAt, done when the call is received so the
// render loop never parses a timestamp.
func (c Call) Started() time.Time { return c.started }

// Fault applies the same definition the server does, so the terminal and the
// web client can never disagree about what counts as a failure.
func (c Call) Fault() bool {
	if c.Error != "" {
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
	default:
		return strconv.Itoa(c.Status)
	}
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
	// exchange that carried many messages, decoded by the API.
	Socket *SocketView `json:"socket"`
	Stream *StreamView `json:"stream"`
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
