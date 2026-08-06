// Command mirador-tui is the terminal client.
//
// It is a second client of the same HTTP API the web interface uses, not a
// second implementation of Mirador. It captures nothing and stores nothing: a
// running mirador does that, and this reads it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"mirador/internal/tui"
)

func main() {
	api := flag.String("api", "http://127.0.0.1:9000", "address of the mirador API")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := tui.NewClient(*api)

	// The stream runs on its own and reconnects by itself, so a mirador that
	// restarts does not leave the terminal silently dead. The buffer absorbs a
	// burst while the model is mid-render; a full buffer drops events, which is
	// the same trade the server makes and for the same reason.
	events := make(chan tui.Call, 256)
	go client.Stream(ctx, events)

	program := tea.NewProgram(
		tui.New(ctx, client, events),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	if _, err := program.Run(); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mirador-tui:", err)
		os.Exit(1)
	}
}
