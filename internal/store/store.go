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
)

// Message is one side of an exchange: the headers, the bytes Mirador kept, and
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
}

type Filter struct {
	Target   string
	Method   string
	Path     string
	Status   int
	Protocol string
	Search   string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int

	// GRPCStatus filters on the gRPC outcome. Nil means no filter; a pointer to
	// zero means "only calls that succeeded", which is a question worth asking.
	GRPCStatus *int32
	// FailedOnly selects transport errors, HTTP 4xx/5xx and non-zero gRPC
	// statuses in one go — the usual reason for opening this tool at all.
	FailedOnly bool
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
	replay_of      INTEGER
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
}

func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(calls)`)
	if err != nil {
		return fmt.Errorf("inspect calls table: %w", err)
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

	for name, ddl := range addedColumns {
		if present[name] {
			continue
		}
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("add column %s: %w", name, err)
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
	if _, err := db.Exec(schema); err != nil {
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
			resp_trailers, grpc_status, grpc_message, replay_of
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Target, c.Protocol, c.Method, c.Path, c.Status, c.ClientAddr,
		c.StartedAt.UnixMicro(), c.Duration.Microseconds(), c.Error,
		reqHeaders, c.Request.Body, c.Request.Size, boolToInt(c.Request.Truncated),
		respHeaders, c.Response.Body, c.Response.Size, boolToInt(c.Response.Truncated),
		respTrailers, nullableInt32(c.GRPCStatus), c.GRPCMessage, nullableInt64(c.ReplayOf),
	)
	if err != nil {
		return 0, fmt.Errorf("insert call: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO calls_fts (rowid, text) VALUES (?, ?)`, id, indexText(c),
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
	if f.GRPCStatus != nil {
		add("grpc_status = ?", *f.GRPCStatus)
	}
	if f.FailedOnly {
		where = append(where, faultPredicate)
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
		       replay_of
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
		)
		if err := rows.Scan(&s.ID, &s.Target, &s.Protocol, &s.Method, &s.Path,
			&s.Status, &startedAt, &durationUS, &s.Error,
			&s.RequestSize, &s.ResponseSize, &grpcStatus, &s.GRPCMessage,
			&replayOf); err != nil {
			return nil, err
		}
		s.StartedAt = time.UnixMicro(startedAt).UTC()
		s.Duration = time.Duration(durationUS) * time.Microsecond
		s.GRPCStatus = int32OrNil(grpcStatus)
		s.ReplayOf = int64OrNil(replayOf)
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
		grpcStatus, replayOf                sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, target, protocol, method, path, status, client_addr, started_at,
		       duration_us, error, req_headers, req_body, req_size, req_truncated,
		       resp_headers, resp_body, resp_size, resp_truncated,
		       resp_trailers, grpc_status, grpc_message, replay_of
		FROM calls WHERE id = ?`, id,
	).Scan(&c.ID, &c.Target, &c.Protocol, &c.Method, &c.Path, &c.Status,
		&c.ClientAddr, &startedAt, &durationUS, &c.Error,
		&reqHeaders, &c.Request.Body, &c.Request.Size, &reqTrunc,
		&respHeader, &c.Response.Body, &c.Response.Size, &respTrunc,
		&respTrailer, &grpcStatus, &c.GRPCMessage, &replayOf)
	if err != nil {
		return nil, err
	}

	c.StartedAt = time.UnixMicro(startedAt).UTC()
	c.Duration = time.Duration(durationUS) * time.Microsecond
	c.Request.Truncated = reqTrunc != 0
	c.Response.Truncated = respTrunc != 0
	c.GRPCStatus = int32OrNil(grpcStatus)
	c.ReplayOf = int64OrNil(replayOf)
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
}

// faultPredicate is one definition of failure, shared by the stats rollup and
// the FailedOnly filter so the rail and the field can never disagree.
const faultPredicate = `(error != '' OR status >= 400 OR (grpc_status IS NOT NULL AND grpc_status != 0))`

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var (
		st             Stats
		oldest, newest sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(started_at), MAX(started_at) FROM calls`,
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
		SELECT target, COUNT(*), SUM(CASE WHEN `+faultPredicate+` THEN 1 ELSE 0 END)
		FROM calls GROUP BY target`)
	if err != nil {
		return st, fmt.Errorf("stats by target: %w", err)
	}
	defer rows.Close()

	st.ByTarget = []TargetStats{}
	for rows.Next() {
		var t TargetStats
		if err := rows.Scan(&t.Target, &t.Calls, &t.Faults); err != nil {
			return st, err
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
func indexText(c *Call) string {
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
