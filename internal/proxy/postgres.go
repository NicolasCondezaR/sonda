package proxy

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/NicolasCondezaR/sonda/internal/config"
	"github.com/NicolasCondezaR/sonda/internal/pgwire"
	"github.com/NicolasCondezaR/sonda/internal/store"
)

// Postgres is the one target that never speaks HTTP, so there is no handshake
// in front of it and no handler to hang it on: the connection is framed
// messages from its first byte.
//
// What a capture is, though, is decided here rather than by the transport. A
// row per connection was the honest first cut and it did not deliver what this
// exists for: behind a pool — which is every real application — one connection
// carries hours of an application's SQL, there is no request to hang it off,
// and an N+1 stays a suspicion. So the split is per statement, on the boundary
// the protocol already gives: a simple query is Q -> results -> ReadyForQuery,
// an extended one is Parse/Bind/Describe/Execute/Sync -> results ->
// ReadyForQuery, and the Z ends the cycle in both. Each capture carries the
// statement's own start and end, which is what lets the existing timing
// correlation hang it under the HTTP request that ran it.
//
// The one thing this path does that no other does is rewrite what it keeps. A
// startup exchange carries the password, and a capture is a file with no
// encryption that an agent can read over MCP, so the credential bytes are
// blanked on their way past. See internal/pgwire/blank.go — which is also what
// frames the stream for the split, so blanking and splitting can never
// disagree about where a message ends.

const (
	// postgresMethod stands in for the HTTP verb on a row that is the
	// connection rather than a statement: one that never ran anything, or one
	// that could not be opened at all.
	postgresMethod = "SESSION"

	// postgresStatementMethod is the ordinary case — one statement cycle.
	postgresStatementMethod = "STATEMENT"
)

// dialTimeout bounds only reaching the database. Nothing bounds the session
// itself: a connection out of a pool stays open for as long as the application
// does, and a debugger that cut it would be the bug.
const dialTimeout = 10 * time.Second

// maxPendingStatements caps how many statements may be waiting for an answer.
// A client that pipelines has to be followed, but nothing may pile up without
// limit against a server that has stopped answering: past this the oldest is
// written as it stands rather than held.
const maxPendingStatements = 16

// A statement that never got its ReadyForQuery is recorded and said to be
// incomplete. Reporting it as a success would be a lie, and dropping it would
// lose exactly the statement someone came looking for: the one that was still
// running when the connection died.
const (
	statementCutShort      = "the connection ended before this statement finished: no ReadyForQuery arrived"
	statementNeverAnswered = "the server never answered this statement and later ones overtook it"
)

// ServePostgres forwards one client connection to the upstream database and
// records a capture per statement while it runs.
func (p *Proxy) ServePostgres(client net.Conn) {
	defer client.Close()
	started := time.Now()

	upstream, err := net.DialTimeout("tcp", p.target.UpstreamURL().Host, dialTimeout)
	if err != nil {
		// A database that cannot be reached is recorded rather than dropped: a
		// connection that never opened, with nothing written down, is the
		// hardest kind of failure to chase.
		p.recorder.Record(&store.Call{
			Target:     p.target.Name,
			Protocol:   config.ProtocolPostgres,
			Method:     postgresMethod,
			ClientAddr: client.RemoteAddr().String(),
			StartedAt:  started,
			Duration:   time.Since(started),
			Error:      fmt.Sprintf("could not reach %s: %v", p.target.Upstream, err),
		})
		return
	}
	defer upstream.Close()

	session := &pgSession{
		proxy:  p,
		client: client.RemoteAddr().String(),
		opened: started,
		limit:  p.maxBody,
	}
	sent := newPGTap(session, true)
	received := newPGTap(session, false)

	// The tap is written before the destination, not after. Blanking must never
	// touch the bytes that are forwarded, and with the tap last a mutation of
	// the shared buffer would arrive too late to be noticed — the wire would
	// stay correct by accident of ordering, and the day someone reordered these
	// two writers the passwords would stop working with no test to say so.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(sent, upstream), client)
		// Half-closing tells the database the client is done, which is what
		// unblocks the other direction and ends the session.
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(received, client), upstream)
		closeWrite(client)
	}()
	wg.Wait()

	session.flush(time.Now())
}

// closeWrite ends one half of a relayed connection. Only a TCP connection can
// do it, and everything reaching here is one; the check exists because the
// interface does not say so.
func closeWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

// pgSegment is one direction's share of one statement cycle: the bytes kept,
// how many crossed, and when.
type pgSegment struct {
	head []byte
	size int64

	// at is when the first byte of the segment arrived, statement when the
	// first byte of a message that asked the server to do work did. They differ
	// on the first cycle of a connection, which also carries the handshake, and
	// the difference is the whole point: a capture timed from the connection
	// opening would not be contained in the request that ran the statement, and
	// the trace tree would not hang it there.
	at        time.Time
	statement time.Time
}

func (s *pgSegment) add(b []byte, now time.Time, limit int64) {
	if len(b) == 0 {
		return
	}
	if s.size == 0 {
		s.at = now
	}
	s.size += int64(len(b))
	if room := limit - int64(len(s.head)); room > 0 {
		if int64(len(b)) > room {
			b = b[:room]
		}
		// append copies, which matters: b aliases the relay's own buffer and
		// that buffer is reused on the next read.
		s.head = append(s.head, b...)
	}
}

func (s pgSegment) message() store.Message {
	return store.Message{Body: s.head, Size: s.size, Truncated: s.size > int64(len(s.head))}
}

// pgSession splits one connection into one capture per statement.
//
// Both relay goroutines write into it, so everything below the mutex is shared
// state. The two directions are cut independently — the client's share of a
// cycle ends at the message that asks for an answer, the server's at the
// ReadyForQuery that gives one — because a rule that depended on which
// goroutine ran first would put the same conversation in different rows on
// different days.
type pgSession struct {
	proxy  *Proxy
	client string
	opened time.Time
	limit  int64

	mu sync.Mutex

	// database is read once off the startup message and carried onto every
	// statement of the connection.
	database string

	// handshake records that the ReadyForQuery which ends authentication has
	// gone past. The first one on a connection is always that one.
	handshake bool

	// copyIn is set while the server is reading a COPY: everything the client
	// sends until it ends is payload of the statement that opened it, not a new
	// statement.
	copyIn bool

	emitted int

	sent     pgSegment   // the client's share of the cycle being written
	queue    []pgSegment // client segments waiting for their ReadyForQuery
	received pgSegment   // the server's share of the cycle being answered
}

// pgMark is one message boundary reported by the blanker.
type pgMark struct {
	typ byte
	end int
}

// pgTap is one direction of the connection. It blanks the credentials before
// anything is kept and splits what is left into cycles.
type pgTap struct {
	session *pgSession
	blanker *pgwire.Blanker
	client  bool
	marks   []pgMark
}

func newPGTap(s *pgSession, client bool) *pgTap {
	t := &pgTap{session: s, blanker: pgwire.NewBlanker(client), client: client}
	t.blanker.OnMessage = func(typ byte, end int) {
		t.marks = append(t.marks, pgMark{typ: typ, end: end})
	}
	return t
}

func (t *pgTap) Write(p []byte) (int, error) {
	now := time.Now()
	t.marks = t.marks[:0]
	blanked := t.blanker.Blank(p)
	if t.client {
		t.session.fromClient(blanked, t.marks, now)
	} else {
		t.session.fromServer(blanked, t.marks, now)
	}
	// The count is of what crossed the wire, whatever was done with the copy: a
	// tap that reported a short write would make io.MultiWriter abort the relay
	// it is observing.
	return len(p), nil
}

func (s *pgSession) fromClient(chunk []byte, marks []pgMark, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	from := 0
	for _, m := range marks {
		// The bytes go on before the message is acted on: a CopyDone belongs to
		// the copy it ends, not to whatever comes after it.
		s.addClient(chunk[from:m.end], now)
		from = m.end

		if m.typ == 0 {
			s.readDatabase()
		}
		if m.typ == 'c' || m.typ == 'f' {
			s.copyIn = false
		}
		if !s.copyIn && pgIsStatement(m.typ) && s.sent.statement.IsZero() {
			s.sent.statement = now
		}
		if pgEndsCycle(m.typ) {
			s.closeClientSegment()
		}
	}
	s.addClient(chunk[from:], now)
}

func (s *pgSession) fromServer(chunk []byte, marks []pgMark, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	from := 0
	for _, m := range marks {
		s.received.add(chunk[from:m.end], now, s.limit)
		from = m.end

		switch m.typ {
		case 'G', 'W':
			// CopyInResponse and CopyBothResponse: what the client sends next is
			// the copy's payload.
			s.copyIn = true
		case 'Z':
			if !s.handshake {
				// The first ReadyForQuery of a connection ends authentication,
				// not a statement. What came before it — the startup
				// parameters, the mechanism that was demanded, the server's
				// settings — belongs to the connection, and it rides in the
				// first statement's capture rather than in a row of its own: a
				// row with no SQL in it is noise on every pooled connection,
				// and dropping the fact that an authentication happened is not
				// something a debugger gets to do.
				s.handshake = true
				continue
			}
			s.complete(now)
		}
	}
	s.received.add(chunk[from:], now, s.limit)
}

// addClient puts bytes on the segment being built — except during a COPY, when
// they continue the statement that opened it. That statement's segment was
// closed the moment the client sent it, so without this the copied rows would
// be filed under the next statement instead.
func (s *pgSession) addClient(b []byte, now time.Time) {
	if len(b) == 0 {
		return
	}
	if s.copyIn && len(s.queue) > 0 {
		s.queue[len(s.queue)-1].add(b, now, s.limit)
		return
	}
	s.sent.add(b, now, s.limit)
}

func (s *pgSession) closeClientSegment() {
	s.queue = append(s.queue, s.sent)
	s.sent = pgSegment{}
	if len(s.queue) > maxPendingStatements {
		oldest := s.queue[0]
		s.queue = s.queue[1:]
		s.emit(oldest, pgSegment{}, time.Now(), statementNeverAnswered)
	}
}

// complete writes the cycle the ReadyForQuery just ended.
func (s *pgSession) complete(now time.Time) {
	var sent pgSegment
	if len(s.queue) > 0 {
		sent, s.queue = s.queue[0], s.queue[1:]
	} else {
		// The server answered something the client never finished asking for.
		// Whatever it has sent so far is the best account of it.
		sent, s.sent = s.sent, pgSegment{}
	}
	s.emit(sent, s.received, now, "")
	s.received = pgSegment{}
	s.copyIn = false
}

// flush writes whatever the connection was still holding when it ended.
func (s *pgSession) flush(end time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	left := mergeSegments(append(s.queue, s.sent), s.limit)
	s.queue, s.sent = nil, pgSegment{}

	// A leftover earns a row when it carries a statement that never finished,
	// when the server said something nobody was answered for, or when this
	// connection has produced no row at all — the last is what keeps a
	// connection that only ever authenticated, or only ever failed, from
	// vanishing. A bare Terminate after the last answered statement is none of
	// those: the client saying goodbye is what the connection closing already
	// says, and a row per connection for it is noise.
	if left.statement.IsZero() && s.received.size == 0 && s.emitted > 0 {
		return
	}

	failure := ""
	if !left.statement.IsZero() {
		failure = statementCutShort
	}
	s.emit(left, s.received, end, failure)
	s.received = pgSegment{}
}

func (s *pgSession) emit(sent, received pgSegment, end time.Time, failure string) {
	started := firstOf(sent.statement, sent.at, received.at, s.opened)
	if end.Before(started) {
		end = started
	}

	// A row with no statement in it is the connection itself, and there is no
	// honest verb for that.
	method := postgresStatementMethod
	if sent.statement.IsZero() {
		method = postgresMethod
	}

	s.emitted++
	s.proxy.recorder.Record(&store.Call{
		Target:     s.proxy.target.Name,
		Protocol:   config.ProtocolPostgres,
		Method:     method,
		Path:       s.database,
		ClientAddr: s.client,
		StartedAt:  started,
		Duration:   end.Sub(started),
		Error:      failure,
		Request:    sent.message(),
		Response:   received.message(),
	})
}

// readDatabase takes the database name off the startup message.
//
// Only the first capture of a connection holds that message, and a statement
// row that could not say which database it ran against would be half a
// reading — so the name is read once here and carried onto every one of them.
func (s *pgSession) readDatabase() {
	if s.database != "" {
		return
	}
	msgs, _ := pgwire.Deframe(s.sent.head, true)
	for _, m := range msgs {
		// Left blank when the client did not name one: Postgres then defaults it
		// to the user name, and repeating that guess here would put a database
		// in the field that may not be the one used.
		if name := m.Parameters["database"]; name != "" {
			s.database = name
			return
		}
	}
}

func mergeSegments(segs []pgSegment, limit int64) pgSegment {
	var out pgSegment
	for _, seg := range segs {
		if out.at.IsZero() {
			out.at = seg.at
		}
		if out.statement.IsZero() {
			out.statement = seg.statement
		}
		out.size += seg.size
		if room := limit - int64(len(out.head)); room > 0 {
			head := seg.head
			if int64(len(head)) > room {
				head = head[:room]
			}
			out.head = append(out.head, head...)
		}
	}
	return out
}

func firstOf(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// pgEndsCycle reports the client messages that ask the server for a
// ReadyForQuery. Everything the client sends between two of them is one
// statement: Query for the simple protocol, Sync for the extended one, and
// FunctionCall for the legacy call the protocol still defines.
func pgEndsCycle(typ byte) bool {
	return typ == 'Q' || typ == 'S' || typ == 'F'
}

// pgIsStatement reports the client messages that mean work was asked for, as
// opposed to the connection being opened, authenticated or closed. Only these
// give a capture a statement's timing and a statement's name.
//
// CopyData, CopyDone and CopyFail are deliberately not among them. They belong
// to the statement that opened the copy, which was closed the moment the client
// sent it — counting them as work asked for would mark the segment that comes
// after the copy as a statement, and the connection would then end with an
// unfinished one that never existed.
func pgIsStatement(typ byte) bool {
	switch typ {
	case 'Q', 'P', 'B', 'E', 'D', 'C', 'F':
		return true
	}
	return false
}
