package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// ServeStdio speaks MCP over a pipe, one JSON message per line.
//
// This is the transport most clients support first: the agent starts the
// server as a child process and talks to it over stdin and stdout. Nothing may
// ever be printed to stdout except protocol messages — a stray log line there
// corrupts the stream and the client sees a parse error with no clue where it
// came from. Logging goes to stderr, which the specification allows precisely
// for this.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// A captured body quoted inside a tool result can be large, and the
	// default 64 KB line limit would truncate the request that asked for it.
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessage)

	// One writer, one lock: replies are written from this loop today, but a
	// half-interleaved JSON line is unrecoverable and the guard is one field.
	var mu sync.Mutex
	encoder := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// The scanner reuses its buffer, and Handle may outlive this
		// iteration; copying is cheaper than the bug.
		raw := make([]byte, len(line))
		copy(raw, line)

		resp := s.Handle(ctx, raw)
		if resp == nil {
			continue
		}

		mu.Lock()
		err := encoder.Encode(resp)
		mu.Unlock()
		if err != nil {
			return err
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// A closed pipe is how this normally ends: the agent exited.
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
