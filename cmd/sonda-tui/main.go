// Command sonda-tui is the terminal client.
//
// It is a second client of the same HTTP API the web interface uses, not a
// second implementation of Sonda. It captures nothing and stores nothing: a
// running sonda does that, and this reads it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"sonda/internal/tui"
)

func main() {
	api := flag.String("api", "http://127.0.0.1:9000", "address of the sonda API")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := tui.NewClient(*api)

	// The stream runs on its own and reconnects by itself, so a sonda that
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
		fmt.Fprintln(os.Stderr, "sonda-tui:", err)
		os.Exit(1)
	}
}
