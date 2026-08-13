package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const maxAutostartLogSize = 5 << 20

func configureLogFile(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve log file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if info, err := os.Stat(absolute); err == nil && info.Size() >= maxAutostartLogSize {
		_ = os.Remove(absolute + ".1")
		if err := os.Rename(absolute, absolute+".1"); err != nil {
			return fmt.Errorf("rotate log file: %w", err)
		}
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	// The process owns one logger for its whole lifetime. Leaving this descriptor
	// to the OS avoids closing it before main records a final startup error.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, file), nil)))
	return nil
}
