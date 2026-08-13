package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/NicolasCondezaR/sonda/internal/autostart"
)

type autostartService interface {
	Install(context.Context, autostart.InstallOptions) (autostart.Status, error)
	Status(context.Context, autostart.ManageOptions) (autostart.Status, error)
	Start(context.Context, autostart.ManageOptions) (autostart.Status, error)
	Stop(context.Context, autostart.ManageOptions) (autostart.Status, error)
	Restart(context.Context, autostart.ManageOptions) (autostart.Status, error)
	Uninstall(context.Context, autostart.ManageOptions) (autostart.Status, error)
}

func runAutostart(argv []string, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAutostartWith(ctx, autostart.New(), argv, stdout, stderr)
}

func runAutostartWith(ctx context.Context, service autostartService, argv []string, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		writeAutostartUsage(stderr)
		return fmt.Errorf("missing autostart command")
	}

	command, rest := argv[0], argv[1:]
	var (
		status autostart.Status
		err    error
	)
	switch command {
	case "install":
		fs := flag.NewFlagSet("sonda autostart install", flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "configuration file (default: ~/.sonda/sonda.yaml)")
		allowNonLoopback := fs.Bool("allow-non-loopback", false, "allow an API listener reachable beyond loopback")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("install takes flags only")
		}
		status, err = service.Install(ctx, autostart.InstallOptions{
			ConfigPath: *configPath, AllowNonLoopback: *allowNonLoopback,
		})
	case "status":
		options, parseErr := parseManageOptions(command, rest, stderr)
		if parseErr != nil {
			return parseErr
		}
		status, err = service.Status(ctx, options)
	case "start":
		options, parseErr := parseManageOptions(command, rest, stderr)
		if parseErr != nil {
			return parseErr
		}
		status, err = service.Start(ctx, options)
	case "stop":
		options, parseErr := parseManageOptions(command, rest, stderr)
		if parseErr != nil {
			return parseErr
		}
		status, err = service.Stop(ctx, options)
	case "restart":
		options, parseErr := parseManageOptions(command, rest, stderr)
		if parseErr != nil {
			return parseErr
		}
		status, err = service.Restart(ctx, options)
	case "uninstall":
		options, parseErr := parseManageOptions(command, rest, stderr)
		if parseErr != nil {
			return parseErr
		}
		status, err = service.Uninstall(ctx, options)
	case "help", "-h", "--help":
		writeAutostartUsage(stdout)
		return nil
	default:
		writeAutostartUsage(stderr)
		return fmt.Errorf("unknown autostart command %q", command)
	}

	writeAutostartStatus(stdout, status)
	return err
}

func parseManageOptions(command string, argv []string, stderr io.Writer) (autostart.ManageOptions, error) {
	fs := flag.NewFlagSet("sonda autostart "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "configuration file used when installing the task (default: ~/.sonda/sonda.yaml)")
	if err := fs.Parse(argv); err != nil {
		return autostart.ManageOptions{}, err
	}
	if fs.NArg() != 0 {
		return autostart.ManageOptions{}, fmt.Errorf("%s takes flags only", command)
	}
	return autostart.ManageOptions{ConfigPath: *configPath}, nil
}

func writeAutostartUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: sonda autostart <install|status|start|stop|restart|uninstall>")
	fmt.Fprintln(w, "       sonda autostart install [-config PATH] [-allow-non-loopback]")
	fmt.Fprintln(w, "       sonda autostart <status|start|stop|restart|uninstall> [-config PATH]")
}

func writeAutostartStatus(w io.Writer, status autostart.Status) {
	fmt.Fprintf(w, "task: %s\n", valueOr(status.TaskName, "unknown"))
	fmt.Fprintf(w, "installed: %s\n", yesNo(status.Installed))
	if !status.Installed {
		if status.Warning != "" {
			fmt.Fprintf(w, "warning: %s\n", strings.TrimSpace(status.Warning))
		}
		return
	}
	fmt.Fprintf(w, "definition: %s\n", map[bool]string{true: "valid", false: "invalid"}[status.TaskValid])
	if status.Problem != "" {
		fmt.Fprintf(w, "problem: %s\n", status.Problem)
	}
	fmt.Fprintf(w, "launcher: %s\n", valueOr(status.Launcher, "unknown"))
	fmt.Fprintf(w, "config: %s (%s)\n", valueOr(status.ConfigPath, "unknown"), map[bool]string{true: "present", false: "missing; defaults apply"}[status.ConfigExists])
	fmt.Fprintf(w, "working_directory: %s\n", valueOr(status.WorkingDirectory, "unknown"))
	fmt.Fprintf(w, "log: %s\n", valueOr(status.LogPath, "unknown"))
	fmt.Fprintf(w, "health: %s %s\n", map[bool]string{true: "active", false: "inactive"}[status.Healthy], status.HealthURL)
	fmt.Fprintf(w, "managed_process: %s\n", map[bool]string{true: "active", false: "inactive"}[status.ManagedProcess])
	if status.AbruptStop {
		fmt.Fprintln(w, "stop: abrupt Task Scheduler fallback was required")
	}
	if status.Warning != "" {
		fmt.Fprintf(w, "warning: %s\n", strings.TrimSpace(status.Warning))
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
