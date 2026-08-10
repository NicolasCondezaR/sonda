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
// messages from its first byte. The shape is otherwise the socket path's — hold
// both directions, pump them through bounded taps, store the raw stream per
// direction and read it back as messages when someone looks.
//
// The one thing this path does that no other does is rewrite what it keeps. A
// startup exchange carries the password, and a capture is a file with no
// encryption that an agent can read over MCP, so the credential bytes are
// blanked in the tap on their way past. See internal/pgwire/blank.go.

// postgresMethod is what stands in for the HTTP verb.
//
// A session is not a request and there is no honest verb for it. "SESSION" says
// what the row actually holds rather than borrowing a word from a protocol this
// call never spoke.
const postgresMethod = "SESSION"

// dialTimeout bounds only reaching the database. Nothing bounds the session
// itself: a connection out of a pool stays open for as long as the application
// does, and a debugger that cut it would be the bug.
const dialTimeout = 10 * time.Second

// ServePostgres forwards one client connection to the upstream database and
// records the conversation when it ends.
func (p *Proxy) ServePostgres(client net.Conn) {
	defer client.Close()
	started := time.Now()

	call := &store.Call{
		Target:     p.target.Name,
		Protocol:   config.ProtocolPostgres,
		Method:     postgresMethod,
		ClientAddr: client.RemoteAddr().String(),
		StartedAt:  started,
	}

	upstream, err := net.DialTimeout("tcp", p.target.UpstreamURL().Host, dialTimeout)
	if err != nil {
		// A database that cannot be reached is recorded rather than dropped: a
		// session that never opened, with nothing written down, is the hardest
		// kind of failure to chase.
		call.Error = fmt.Sprintf("could not reach %s: %v", p.target.Upstream, err)
		call.Duration = time.Since(started)
		p.recorder.Record(call)
		return
	}
	defer upstream.Close()

	sent := &tap{limit: p.maxBody, blank: pgwire.NewBlanker(true).Blank}
	received := &tap{limit: p.maxBody, blank: pgwire.NewBlanker(false).Blank}

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

	call.Request.Body, call.Request.Size = sent.result()
	call.Request.Truncated = call.Request.Size > int64(len(call.Request.Body))
	call.Response.Body, call.Response.Size = received.result()
	call.Response.Truncated = call.Response.Size > int64(len(call.Response.Body))
	call.Duration = time.Since(started)
	p.recorder.Record(call)
}

// closeWrite ends one half of a relayed connection. Only a TCP connection can
// do it, and everything reaching here is one; the check exists because the
// interface does not say so.
func closeWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
