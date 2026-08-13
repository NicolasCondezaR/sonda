package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

type stdioRequestKey struct{}

func isStdioRequest(ctx context.Context) bool {
	allowed, _ := ctx.Value(stdioRequestKey{}).(bool)
	return allowed
}

// ServeStdio speaks MCP over a pipe, one JSON message per line.
//
// This is the transport most clients support first: the agent starts the
// server as a child process and talks to it over stdin and stdout. Nothing may
// ever be printed to stdout except protocol messages — a stray log line there
// corrupts the stream and the client sees a parse error with no clue where it
// came from. Logging goes to stderr, which the specification allows precisely
// for this.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	// Filesystem-backed tool arguments are intentionally available only on
	// this transport. The HTTP handler never sets this marker, even if someone
	// accidentally constructed it from a stdio-capable Server.
	ctx = context.WithValue(ctx, stdioRequestKey{}, true)

	// A captured body quoted inside a tool result can be large, and the
	// default 64 KB line limit would truncate the request that asked for it.
	reader := bufio.NewReaderSize(in, 64<<10)

	// One writer, one lock: replies are written from this loop today, but a
	// half-interleaved JSON line is unrecoverable and the guard is one field.
	var mu sync.Mutex
	encoder := json.NewEncoder(out)

	write := func(resp *response) error {
		mu.Lock()
		defer mu.Unlock()
		return encoder.Encode(resp)
	}

	for {
		raw, oversized, err := readLine(reader, maxMessage)
		if oversized {
			// The line is gone either way, so there is no id to answer under.
			// What matters is that the client is told: this used to end the
			// loop and the process with it, so an upload_schemas above about
			// three megabytes — four thirds of that once base64-encoded — did
			// not fail, it killed the server, and the client saw Sonda die.
			if err := write(failure(nil, codeInvalidRequest,
				"that message is over the %d byte limit of the stdio transport and was dropped; for upload_schemas, pass the local descriptor path instead of putting its base64 bytes in JSON",
				maxMessage)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			// A closed pipe is how this normally ends: the agent exited.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(raw) == 0 {
			continue
		}

		resp := s.Handle(ctx, raw)
		if resp == nil {
			continue
		}
		if err := write(resp); err != nil {
			return err
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// readLine reads one line, refusing anything past limit rather than growing
// without bound or failing the stream for good.
//
// bufio.Scanner cannot do this: ErrTooLong is terminal, so one enormous line
// makes every message after it unreadable. Discarding to the next newline is
// what lets the session carry on.
func readLine(r *bufio.Reader, limit int) (line []byte, oversized bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			oversized = true
			line = nil
		}
		if !oversized {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && len(line) == 0 && !oversized {
			return nil, false, err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, oversized, err
		}
		// A last line with no newline is still a message.
		return trimEOL(line), oversized, nil
	}
}

func trimEOL(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}
