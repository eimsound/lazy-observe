package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newPreStopCommand() *cobra.Command {
	delay := 5 * time.Second
	command := &cobra.Command{
		Use:    "pre-stop",
		Short:  "Signal PID 1 and wait before a container is stopped",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if delay < 0 {
				return fmt.Errorf("pre-stop delay must not be negative: %s", delay)
			}
			return runPreStop(delay)
		},
	}
	command.Flags().DurationVar(&delay, "delay", delay, "delay after marking PID 1 unready and before SIGTERM")
	return command
}

func runPreStop(delay time.Duration) error {
	return runPreStopWith(os.Getpid(), 1, delay, syscall.Kill, time.Sleep)
}

func runPreStopWith(
	selfPID, targetPID int,
	delay time.Duration,
	killFn func(int, syscall.Signal) error,
	sleepFn func(time.Duration),
) error {
	if selfPID == targetPID {
		return errors.New("pre-stop must run as a lifecycle hook, not PID 1")
	}
	// First mark PID 1 unready via SIGUSR1 so readiness probes and kube-proxy
	// observe 503 while the listener keeps serving in-flight and still-routed
	// connections for the full pre-stop window.
	if err := killFn(targetPID, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signal server PID %d to begin drain: %w", targetPID, err)
	}
	if delay > 0 {
		sleepFn(delay)
	}
	if err := killFn(targetPID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal server PID %d to stop: %w", targetPID, err)
	}
	return nil
}
