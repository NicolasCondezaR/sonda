// Package store persists captured calls in SQLite.
//
// The bytes on the wire are the record. Nothing here parses, pretty-prints or
// re-serializes a body: a body goes in as the exact bytes that crossed the
// proxy and comes out the same way. Decoding is a view computed at display
// time, which is what makes replay faithful and lets a capture become readable
// later, once its schema is available.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/graphql"
	"github.com/NicolasCondezaR/sonda/internal/pgwire"
)

// Message is one side of an exchange: the headers, the bytes Sonda kept, and
// how many bytes actually crossed the wire.
type Message struct {
	Headers   http.Header
	Body      []byte
	Size      int64
	Truncated bool
}

type Call struct {
	ID         int64
	Target     string
	Protocol   string
	Method     string
	Path       string
	Status     int
	ClientAddr string
	StartedAt  time.Time
	Duration   time.Duration
	Error      string
	Request    Message
	Response   Message

	// Trailers matter for gRPC and almost nowhere else: the call's real
	// outcome arrives there, after the body.
	ResponseTrailers http.Header

	// GRPCStatus is nil for anything that is not a gRPC call. Zero is a
	// meaningful value — it is OK — so it cannot double as "absent".
	GRPCStatus  *int32
	GRPCMessage string

	// ReplayOf links a call back to the capture it was replayed from, which is
	// what makes "did the fix work?" a diff rather than a memory exercise.
	ReplayOf *int64

	// Project is the group the capture was taken under. Without it, switching
	// projects would pour one system's traffic into another's field.
	Project string

	// TraceID is whatever the request carried to identify the wider operation
	// it belongs to. Read, never invented: an id Sonda made up would group
	// calls by nothing and look exactly as authoritative as a real one.
	TraceID string

	// StubOf points at the capture this answer was replayed from, when the
	// service was not called at all. It is the difference between a recording
	// and a fact, and everything that displays a call has to be able to tell
	// them apart.
	StubOf *int64

	// Injected says Sonda broke this call on purpose. Without it the field
	// would show the tool's own interference as if the service had produced
	// it, and someone would go hunting a bug that does not exist.
	Injected bool

	// GraphQLOp and GraphQLErrors are the only two things read out of a body
	// and kept as columns, and both exist because a listing carries no bodies.
	//
	// Every GraphQL call is the same POST to the same path, so without the
	// operation the field shows a service as one repeated call. And a GraphQL
	// failure arrives under HTTP 200, so without the error count the SQL that
	// decides what counts as a fault cannot see it — and the field would show
	// the failure someone came here to find as a success. Both are derived on
	// insert; the bodies themselves go in untouched.
	GraphQLOp     string
	GraphQLErrors int

	// PostgresSummary and PostgresErrors exist for the two reasons GraphQLOp
	// and GraphQLErrors do, and they are the same two reasons.
	//
	// A Postgres capture has no method and no path worth showing — every
	// session to a database looks identical from outside — so without the
	// summary the field shows a service as one repeated call. And a failed
	// statement is an ErrorResponse inside the stream, not a status code, so
	// without the error count the SQL that decides what counts as a fault
	// cannot see it. Both are read out of the stored stream on insert; the
	// stream itself goes in untouched.
	PostgresSummary string
	PostgresErrors  int

	// TLS, UpstreamTLS and UpstreamInsecure are how this call was encrypted, and
	// they are three separate facts because "the client's half was encrypted"
	// and "the service's half was" are answered by different sides of the proxy.
	//
	// UpstreamInsecure is the one that matters most and the reason the other two
	// are stored beside it: a reader looking at a captured response must never
	// have to go and check the configuration to find out whether the identity of
	// whoever sent it was ever checked — least of all months later, when the
	// service may have been reconfigured since.
	TLS              bool
	UpstreamTLS      bool
	UpstreamInsecure bool
}

// Summary is the list view. It deliberately carries no bodies: a listing of a
// few hundred calls with payloads attached is unusable and slow.
type Summary struct {
	ID           int64
	Target       string
	Protocol     string
	Method       string
	Path         string
	Status       int
	StartedAt    time.Time
	Duration     time.Duration
	Error        string
	RequestSize  int64
	ResponseSize int64
	GRPCStatus   *int32
	GRPCMessage  string
	ReplayOf     *int64
	Project      string
	TraceID      string
	StubOf       *int64
	Injected     bool

	GraphQLOp     string
	GraphQLErrors int

	PostgresSummary string
	PostgresErrors  int

	TLS              bool
	UpstreamTLS      bool
	UpstreamInsecure bool
}

type Filter struct {
	Target   string
	Method   string
	Path     string
	Status   int
	Protocol string
	Search   string
	Project  string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int

	// GRPCStatus filters on the gRPC outcome. Nil means no filter; a pointer to
	// zero means "only calls that succeeded", which is a question worth asking.
	GRPCStatus *int32
	// Failed selects on faultPredicate. Nil is no filter, true is only the
	// calls that failed — the usual reason for opening this tool at all — and
	// false is only the ones that did not.
	//
	// A pointer, and not a bool, because "show me what still works" is a real
	// question and the natural contrast to the failures listing. As a plain
	// bool it was indistinguishable from absent, so it silently returned
	// everything and answered the opposite of what was asked.
	Failed *bool
}

const (
	defaultLimit = 100
	maxLimit     = 1000

	// Only the head of each body is indexed for full-text search. The whole
	// body is still stored and still returned on the detail view.
	ftsTextLimit = 32 << 10
)

const schema = `
CREATE TABLE IF NOT EXISTS calls (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	target         TEXT    NOT NULL,
	protocol       TEXT    NOT NULL,
	method         TEXT    NOT NULL,
	path           TEXT    NOT NULL,
	status         INTEGER NOT NULL,
	client_addr    TEXT    NOT NULL,
	started_at     INTEGER NOT NULL,
	duration_us    INTEGER NOT NULL,
	error          TEXT    NOT NULL,
	req_headers    TEXT    NOT NULL,
	req_body       BLOB,
	req_size       INTEGER NOT NULL,
	req_truncated  INTEGER NOT NULL,
	resp_headers   TEXT    NOT NULL,
	resp_body      BLOB,
	resp_size      INTEGER NOT NULL,
	resp_truncated INTEGER NOT NULL,
	resp_trailers  TEXT    NOT NULL DEFAULT '{}',
	grpc_status    INTEGER,
	grpc_message   TEXT    NOT NULL DEFAULT '',
	replay_of      INTEGER,
	project        TEXT    NOT NULL DEFAULT '',
	graphql_op     TEXT    NOT NULL DEFAULT '',
	graphql_errors INTEGER NOT NULL DEFAULT 0,
	pg_summary     TEXT    NOT NULL DEFAULT '',
	pg_errors      INTEGER NOT NULL DEFAULT 0,
	tls              INTEGER NOT NULL DEFAULT 0,
	upstream_tls     INTEGER NOT NULL DEFAULT 0,
	upstream_insecure INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS calls_started_at_idx ON calls(started_at DESC);
CREATE INDEX IF NOT EXISTS calls_target_idx     ON calls(target, id DESC);
CREATE VIRTUAL TABLE IF NOT EXISTS calls_fts USING fts5(text);
`

// addedColumns brings a database created by an earlier version up to date.
// They are also in the CREATE TABLE above, so a fresh database skips all of
// them; this exists only so an existing capture file is not thrown away.
var addedColumns = map[string]string{
	"resp_trailers": `ALTER TABLE calls ADD COLUMN resp_trailers TEXT NOT NULL DEFAULT '{}'`,
	"grpc_status":   `ALTER TABLE calls ADD COLUMN grpc_status INTEGER`,
	"grpc_message":  `ALTER TABLE calls ADD COLUMN grpc_message TEXT NOT NULL DEFAULT ''`,
	"replay_of":     `ALTER TABLE calls ADD COLUMN replay_of INTEGER`,
	"project":       `ALTER TABLE calls ADD COLUMN project TEXT NOT NULL DEFAULT ''`,
	"trace_id":      `ALTER TABLE calls ADD COLUMN trace_id TEXT NOT NULL DEFAULT ''`,
	"stub_of":       `ALTER TABLE calls ADD COLUMN stub_of INTEGER`,
	"injected":      `ALTER TABLE calls ADD COLUMN injected INTEGER NOT NULL DEFAULT 0`,
	// Calls captured before this column existed report no operation and no
	// errors. Backfilling would mean re-reading every stored body to write a
	// value nobody measured at the time, and an old capture that quietly
	// changed outcome is worse than one that is honestly blank.
	"graphql_op":     `ALTER TABLE calls ADD COLUMN graphql_op TEXT NOT NULL DEFAULT ''`,
	"graphql_errors": `ALTER TABLE calls ADD COLUMN graphql_errors INTEGER NOT NULL DEFAULT 0`,
	"pg_summary":     `ALTER TABLE calls ADD COLUMN pg_summary TEXT NOT NULL DEFAULT ''`,
	"pg_errors":      `ALTER TABLE calls ADD COLUMN pg_errors INTEGER NOT NULL DEFAULT 0`,
	// A capture taken before these columns existed reports plaintext and
	// unverified-nothing, which is what it was: Sonda could not terminate TLS at
	// the time and never skipped a check nobody could ask it to skip.
	"tls":               `ALTER TABLE calls ADD COLUMN tls INTEGER NOT NULL DEFAULT 0`,
	"upstream_tls":      `ALTER TABLE calls ADD COLUMN upstream_tls INTEGER NOT NULL DEFAULT 0`,
	"upstream_insecure": `ALTER TABLE calls ADD COLUMN upstream_insecure INTEGER NOT NULL DEFAULT 0`,
}

func migrate(db *sql.DB) error {
	if err := addColumns(db, "calls", addedColumns); err != nil {
		return err
	}
	return addColumns(db, "services", addedServiceColumns)
}

func addColumns(db *sql.DB, table string, wanted map[string]string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s table: %w", table, err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			deflt            sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &deflt, &pk); err != nil {
			rows.Close()
			return err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for name, ddl := range wanted {
		if present[name] {
			continue
		}
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("add column %s.%s: %w", table, name, err)
		}
	}
	return nil
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// One writer avoids SQLITE_BUSY on the recorder path; reads are cheap enough
	// to share it in a tool that observes a single developer's local stack.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + projectSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Insert(ctx context.Context, c *Call) (int64, error) {
	reqHeaders, err := encodeHeaders(c.Request.Headers)
	if err != nil {
		return 0, err
	}
	respHeaders, err := encodeHeaders(c.Response.Headers)
	if err != nil {
		return 0, err
	}
	respTrailers, err := encodeHeaders(c.ResponseTrailers)
	if err != nil {
		return 0, err
	}

	// Derived here rather than by each caller: the proxy, the stub path and the
	// fault path all record through this one function, and a reading taken in
	// three places is a reading that disagrees with itself.
	describeGraphQL(c)
	postgresText := describePostgres(c)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO calls (
			target, protocol, method, path, status, client_addr, started_at,
			duration_us, error, req_headers, req_body, req_size, req_truncated,
			resp_headers, resp_body, resp_size, resp_truncated,
			resp_trailers, grpc_status, grpc_message, replay_of, project, trace_id,
			stub_of, injected, graphql_op, graphql_errors, pg_summary, pg_errors,
			tls, upstream_tls, upstream_insecure
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Target, c.Protocol, c.Method, c.Path, c.Status, c.ClientAddr,
		c.StartedAt.UnixMicro(), c.Duration.Microseconds(), c.Error,
		reqHeaders, c.Request.Body, c.Request.Size, boolToInt(c.Request.Truncated),
		respHeaders, c.Response.Body, c.Response.Size, boolToInt(c.Response.Truncated),
		respTrailers, nullableInt32(c.GRPCStatus), c.GRPCMessage, nullableInt64(c.ReplayOf),
		c.Project, c.TraceID, nullableInt64(c.StubOf), boolToInt(c.Injected),
		c.GraphQLOp, c.GraphQLErrors, c.PostgresSummary, c.PostgresErrors,
		boolToInt(c.TLS), boolToInt(c.UpstreamTLS), boolToInt(c.UpstreamInsecure),
	)
	if err != nil {
		return 0, fmt.Errorf("insert call: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO calls_fts (rowid, text) VALUES (?, ?)`, id, indexText(c, postgresText),
	); err != nil {
		return 0, fmt.Errorf("index call: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	c.ID = id
	return id, nil
}

func (s *Store) List(ctx context.Context, f Filter) ([]Summary, error) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		where = append(where, clause)
		args = append(args, value)
	}

	if f.Target != "" {
		add("target = ?", f.Target)
	}
	if f.Method != "" {
		add("method = ?", strings.ToUpper(f.Method))
	}
	if f.Path != "" {
		add("path LIKE ?", "%"+escapeLike(f.Path)+"%")
		where[len(where)-1] += ` ESCAPE '\'`
	}
	if f.Status != 0 {
		add("status = ?", f.Status)
	}
	if f.Protocol != "" {
		add("protocol = ?", f.Protocol)
	}
	if f.Project != "" {
		add("project = ?", f.Project)
	}
	if f.GRPCStatus != nil {
		add("grpc_status = ?", *f.GRPCStatus)
	}
	if f.Failed != nil {
		if *f.Failed {
			where = append(where, faultPredicate)
		} else {
			// Negated rather than spelled out a second time: two hand-written
			// halves of one definition drift, and the half nobody looks at is
			// the one that goes wrong. Every column it reads is NOT NULL except
			// grpc_status, which is guarded by its own IS NOT NULL, so the
			// predicate is never NULL and NOT is a true complement here.
			where = append(where, "NOT "+faultPredicate)
		}
	}
	if !f.Since.IsZero() {
		add("started_at >= ?", f.Since.UnixMicro())
	}
	if !f.Until.IsZero() {
		add("started_at <= ?", f.Until.UnixMicro())
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	if f.Search != "" {
		add("id IN (SELECT rowid FROM calls_fts WHERE calls_fts MATCH ?)", ftsPhrase(f.Search))
	}

	query := `
		SELECT id, target, protocol, method, path, status, started_at,
		       duration_us, error, req_size, resp_size, grpc_status, grpc_message,
		       replay_of, project, trace_id, stub_of, injected, graphql_op, graphql_errors,
		       pg_summary, pg_errors, tls, upstream_tls, upstream_insecure
		FROM calls`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, clampLimit(f.Limit))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list calls: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		var (
			s          Summary
			startedAt  int64
			durationUS int64
			grpcStatus sql.NullInt64
			replayOf   sql.NullInt64
			stubOf     sql.NullInt64
			injected   int

			terminated, upstreamTLS, insecure int
		)
		if err := rows.Scan(&s.ID, &s.Target, &s.Protocol, &s.Method, &s.Path,
			&s.Status, &startedAt, &durationUS, &s.Error,
			&s.RequestSize, &s.ResponseSize, &grpcStatus, &s.GRPCMessage,
			&replayOf, &s.Project, &s.TraceID, &stubOf, &injected,
			&s.GraphQLOp, &s.GraphQLErrors, &s.PostgresSummary, &s.PostgresErrors,
			&terminated, &upstreamTLS, &insecure); err != nil {
			return nil, err
		}
		s.StartedAt = time.UnixMicro(startedAt).UTC()
		s.Duration = time.Duration(durationUS) * time.Microsecond
		s.GRPCStatus = int32OrNil(grpcStatus)
		s.ReplayOf = int64OrNil(replayOf)
		s.StubOf = int64OrNil(stubOf)
		s.Injected = injected != 0
		s.TLS = terminated != 0
		s.UpstreamTLS = upstreamTLS != 0
		s.UpstreamInsecure = insecure != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id int64) (*Call, error) {
	var (
		c                                   Call
		startedAt, durationUS               int64
		reqHeaders, respHeader, respTrailer string
		reqTrunc, respTrunc                 int
		grpcStatus, replayOf, stubOf        sql.NullInt64
		injectedFlag                        int

		terminated, upstreamTLS, insecure int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, target, protocol, method, path, status, client_addr, started_at,
		       duration_us, error, req_headers, req_body, req_size, req_truncated,
		       resp_headers, resp_body, resp_size, resp_truncated,
		       resp_trailers, grpc_status, grpc_message, replay_of, project, trace_id,
		       stub_of, injected, graphql_op, graphql_errors, pg_summary, pg_errors,
		       tls, upstream_tls, upstream_insecure
		FROM calls WHERE id = ?`, id,
	).Scan(&c.ID, &c.Target, &c.Protocol, &c.Method, &c.Path, &c.Status,
		&c.ClientAddr, &startedAt, &durationUS, &c.Error,
		&reqHeaders, &c.Request.Body, &c.Request.Size, &reqTrunc,
		&respHeader, &c.Response.Body, &c.Response.Size, &respTrunc,
		&respTrailer, &grpcStatus, &c.GRPCMessage, &replayOf, &c.Project, &c.TraceID, &stubOf, &injectedFlag,
		&c.GraphQLOp, &c.GraphQLErrors, &c.PostgresSummary, &c.PostgresErrors,
		&terminated, &upstreamTLS, &insecure)
	if err != nil {
		return nil, err
	}

	c.StartedAt = time.UnixMicro(startedAt).UTC()
	c.Duration = time.Duration(durationUS) * time.Microsecond
	c.Request.Truncated = reqTrunc != 0
	c.Response.Truncated = respTrunc != 0
	c.GRPCStatus = int32OrNil(grpcStatus)
	c.ReplayOf = int64OrNil(replayOf)
	c.StubOf = int64OrNil(stubOf)
	c.Injected = injectedFlag != 0
	c.TLS = terminated != 0
	c.UpstreamTLS = upstreamTLS != 0
	c.UpstreamInsecure = insecure != 0
	if c.Request.Headers, err = decodeHeaders(reqHeaders); err != nil {
		return nil, err
	}
	if c.Response.Headers, err = decodeHeaders(respHeader); err != nil {
		return nil, err
	}
	if c.ResponseTrailers, err = decodeHeaders(respTrailer); err != nil {
		return nil, err
	}
	return &c, nil
}

type Stats struct {
	Calls   int64     `json:"calls"`
	Oldest  time.Time `json:"oldest,omitzero"`
	Newest  time.Time `json:"newest,omitzero"`
	Dropped int64     `json:"dropped"`

	// ByTarget is what the channel rail reads. It is deliberately unfiltered:
	// the rail answers "is this service healthy", and that question cannot be
	// answered by counting only the rows the current filter let through.
	ByTarget []TargetStats `json:"by_target"`
}

type TargetStats struct {
	Target string `json:"target"`
	Calls  int64  `json:"calls"`
	Faults int64  `json:"faults"`

	// Last is when this target last captured anything. "Nothing since I started
	// the client" and "nothing in the last two hours" are different findings,
	// and a count alone cannot tell them apart.
	Last time.Time `json:"last,omitzero"`
}

// faultPredicate is one definition of failure, shared by the stats rollup and
// the Failed filter — in both directions — so the rail and the field can never
// disagree.
//
// The last three clauses are the same problem three times: gRPC, GraphQL and
// Postgres all report failure somewhere other than the HTTP status — and a
// Postgres session has no HTTP status at all — so a definition that trusted the
// status alone would show the failure someone opened this tool to find as a
// success.
const faultPredicate = `(error != '' OR status >= 400 OR (grpc_status IS NOT NULL AND grpc_status != 0) OR graphql_errors > 0 OR pg_errors > 0)`

func (s *Store) Stats(ctx context.Context, project string) (Stats, error) {
	var (
		st             Stats
		oldest, newest sql.NullInt64
		where          string
		args           []any
	)
	if project != "" {
		where, args = " WHERE project = ?", []any{project}
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(started_at), MAX(started_at) FROM calls`+where, args...,
	).Scan(&st.Calls, &oldest, &newest)
	if err != nil {
		return st, err
	}
	if oldest.Valid {
		st.Oldest = time.UnixMicro(oldest.Int64).UTC()
	}
	if newest.Valid {
		st.Newest = time.UnixMicro(newest.Int64).UTC()
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT target, COUNT(*), SUM(CASE WHEN `+faultPredicate+` THEN 1 ELSE 0 END), MAX(started_at)
		FROM calls`+where+` GROUP BY target`, args...)
	if err != nil {
		return st, fmt.Errorf("stats by target: %w", err)
	}
	defer rows.Close()

	st.ByTarget = []TargetStats{}
	for rows.Next() {
		var (
			t    TargetStats
			last sql.NullInt64
		)
		if err := rows.Scan(&t.Target, &t.Calls, &t.Faults, &last); err != nil {
			return st, err
		}
		if last.Valid {
			t.Last = time.UnixMicro(last.Int64).UTC()
		}
		st.ByTarget = append(st.ByTarget, t)
	}
	return st, rows.Err()
}

// Prune enforces retention. Both limits are applied on every pass: age first,
// then the row cap, so a burst of traffic cannot outrun the ceiling.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration, maxCalls int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	cutoff := time.Now().Add(-maxAge).UnixMicro()
	byAge, err := deleteReturning(ctx, tx, `DELETE FROM calls WHERE started_at < ? RETURNING id`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune by age: %w", err)
	}

	// The id of the oldest row allowed to survive the row cap; everything below
	// it goes. NULL means the table is already under the cap.
	var threshold sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM calls ORDER BY id DESC LIMIT 1 OFFSET ?`, maxCalls-1,
	).Scan(&threshold); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("prune by count: %w", err)
	}

	var byCount int64
	if threshold.Valid {
		byCount, err = deleteReturning(ctx, tx, `DELETE FROM calls WHERE id < ? RETURNING id`, threshold.Int64)
		if err != nil {
			return 0, fmt.Errorf("prune by count: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return byAge + byCount, nil
}

// deleteReturning removes rows and mirrors the deletion into the search index.
// RETURNING gives the exact ids, so the two tables cannot drift.
func deleteReturning(ctx context.Context, tx *sql.Tx, query string, arg any) (int64, error) {
	rows, err := tx.QueryContext(ctx, query, arg)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM calls_fts WHERE rowid = ?`, id); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), nil
}

func encodeHeaders(h http.Header) (string, error) {
	if len(h) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("encode headers: %w", err)
	}
	return string(b), nil
}

func decodeHeaders(s string) (http.Header, error) {
	h := http.Header{}
	if s == "" || s == "{}" {
		return h, nil
	}
	if err := json.Unmarshal([]byte(s), &h); err != nil {
		return nil, fmt.Errorf("decode headers: %w", err)
	}
	return h, nil
}

// indexText builds the searchable projection of a call.
//
// Encoding is not allowed to hide a call. A payload that is mostly readable but
// carries a stray byte — a latin-1 accent from a service that never got its
// charset right, which is the normal case in a Spanish-language stack — still
// gets indexed, with the bad bytes replaced. Refusing to index it would make
// the call unfindable because of one character, and finding calls is the entire
// point of the tool.
//
// Genuinely binary payloads are still skipped: indexing them yields noise
// rather than matches, and bloats the index with tokens nobody searches for.
func indexText(c *Call, extra string) string {
	var b strings.Builder
	b.WriteString(c.Target)
	b.WriteByte(' ')
	b.WriteString(c.Method)
	b.WriteByte(' ')
	b.WriteString(c.Path)
	if c.Error != "" {
		b.WriteByte(' ')
		b.WriteString(c.Error)
	}
	if c.GRPCMessage != "" {
		b.WriteByte(' ')
		b.WriteString(c.GRPCMessage)
	}
	// A Postgres stream is full of NUL bytes and length prefixes, so isIndexable
	// rejects it as binary and everything readable inside it would be lost to
	// search. extra is that reading, taken while the stream was decoded for the
	// summary: the statement in full rather than the summary's first ninety
	// characters, the parameters bound to it, and what the server answered.
	if extra != "" {
		b.WriteByte(' ')
		if len(extra) > ftsTextLimit {
			extra = extra[:ftsTextLimit]
		}
		b.WriteString(extra)
	}
	for _, body := range [][]byte{c.Request.Body, c.Response.Body} {
		if !isIndexable(body) {
			continue
		}
		if len(body) > ftsTextLimit {
			body = body[:ftsTextLimit]
		}
		b.WriteByte(' ')
		// Invalid sequences become spaces, which the tokenizer treats as
		// separators, so the readable tokens around them survive.
		b.WriteString(strings.ToValidUTF8(string(body), " "))
	}
	return b.String()
}

// binaryRatioLimit is the share of non-printable bytes above which a payload is
// treated as binary rather than as text with encoding problems.
const binaryRatioLimit = 0.3

func isIndexable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	sample := b
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	nonPrintable := 0
	for _, c := range sample {
		if c == 0 {
			return false // a NUL byte is the one reliable marker of binary content
		}
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(len(sample)) <= binaryRatioLimit
}

// ftsPhrase wraps the query as a single FTS5 phrase. Search terms in this tool
// are paths, ids and JSON fragments full of characters FTS5 reads as operators;
// treating the input as a literal phrase is what a developer expects from a
// search box.
func ftsPhrase(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	default:
		return n
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableInt32(v *int32) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func int64OrNil(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func int32OrNil(v sql.NullInt64) *int32 {
	if !v.Valid {
		return nil
	}
	n := int32(v.Int64)
	return &n
}

// MatchForStub finds a recorded answer for a request the service is not going
// to be asked.
//
// The ordering is the whole design. An identical request body wins outright,
// because that is the difference between replaying "the response to GetOrder"
// and replaying "the response to GetOrder(ORD-1)" — and a test that gets the
// wrong order back is worse off than one that got an error. Failing that, the
// most recent call to the same method and path is the best available guess.
//
// Captures that were themselves stubbed are excluded. Without that, turning
// stubbing on and leaving it on would slowly feed Sonda its own answers, and
// the recording would drift from anything a service ever really said.
func (s *Store) MatchForStub(ctx context.Context, target, method, path string, body []byte) (*Call, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM calls
		WHERE target = ? AND method = ? AND path = ? AND stub_of IS NULL
		ORDER BY (req_body = ?) DESC, id DESC
		LIMIT 1`,
		target, method, path, body,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look for a recorded answer: %w", err)
	}
	return s.Get(ctx, id)
}

// Summary is the listing form of a call, without the bodies.
//
// It exists so the mapping lives in one place. It used to be written out by
// hand wherever a detail had to be shown as a summary, and a hand-written copy
// of a struct silently drops whatever field was added last — which is exactly
// how stub_of reached the listing and never reached the detail.
func (c *Call) Summary() Summary {
	return Summary{
		ID: c.ID, Target: c.Target, Protocol: c.Protocol, Method: c.Method,
		Path: c.Path, Status: c.Status, StartedAt: c.StartedAt,
		Duration: c.Duration, Error: c.Error,
		RequestSize: c.Request.Size, ResponseSize: c.Response.Size,
		GRPCStatus: c.GRPCStatus, GRPCMessage: c.GRPCMessage,
		ReplayOf: c.ReplayOf, Project: c.Project,
		TraceID: c.TraceID, StubOf: c.StubOf, Injected: c.Injected,
		GraphQLOp: c.GraphQLOp, GraphQLErrors: c.GraphQLErrors,
		PostgresSummary: c.PostgresSummary, PostgresErrors: c.PostgresErrors,
		TLS: c.TLS, UpstreamTLS: c.UpstreamTLS, UpstreamInsecure: c.UpstreamInsecure,
	}
}

// describeGraphQL reads the operation and the error count off a call's bodies.
//
// It writes nothing back into either body: the record stays the exact bytes
// that crossed the proxy, and these two values are a reading taken from them,
// the same way the search index is.
func describeGraphQL(c *Call) {
	e := graphql.Decode(c.Method, c.Request.Body, c.Response.Body)
	if e == nil {
		return
	}
	c.GraphQLOp = e.Label()
	c.GraphQLErrors = e.Errors()
}

// describePostgres reads the one-line summary and the error count off a
// captured statement, and returns the text worth putting in the search index.
//
// It runs here for the same reason describeGraphQL does: every writer routes
// through Insert, and a reading taken in three places is a reading that
// disagrees with itself. It writes nothing back into either stream — the record
// stays the exact bytes that crossed, minus the credentials the tap already
// blanked on the way in.
func describePostgres(c *Call) string {
	if c.Protocol != config.ProtocolPostgres {
		return ""
	}
	sent, _ := pgwire.Deframe(c.Request.Body, true)
	received, _ := pgwire.Deframe(c.Response.Body, false)

	// The proxy already knows the database and puts it on every statement of a
	// connection, only the first of which carries the startup message. This is
	// the fallback for a capture that arrived without one.
	if c.Path == "" {
		for _, m := range sent {
			if m.Kind == "startup" {
				// Left blank when the client did not name one: Postgres then
				// defaults it to the user name, and repeating that guess here
				// would put a database in the field that may not be the one
				// used.
				c.Path = m.Parameters["database"]
				break
			}
		}
	}
	for _, m := range received {
		if m.Kind == "error_response" {
			c.PostgresErrors++
		}
	}

	// Both directions in one reading: the statement comes from the client and
	// its outcome from the server, and either alone is half the answer.
	all := make([]pgwire.Message, 0, len(sent)+len(received))
	all = append(append(all, sent...), received...)
	c.PostgresSummary = pgwire.Summarise(all)
	return postgresIndexText(all)
}

// postgresIndexText is everything readable in a statement, for the search
// index: the SQL in full, the values bound to it, the command tags and what the
// server complained about.
//
// The summary alone is not enough. It is one line cut at ninety characters, and
// searching for a table named halfway down a formatted statement would miss it.
func postgresIndexText(msgs []pgwire.Message) string {
	var b strings.Builder
	write := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	for _, m := range msgs {
		write(m.SQL)
		write(m.Tag)
		write(m.Message)
		write(m.Code)
		write(m.Detail)
		for _, p := range m.Params {
			write(p.Text)
		}
	}
	return b.String()
}
