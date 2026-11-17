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
	command.Flags().DurationVar(&delay, "delay", delay, "delay after signaling PID 1")
	return command
}

func runPreStop(delay time.Duration) error {
	if os.Getpid() == 1 {
		return errors.New("pre-stop must run as a lifecycle hook, not PID 1")
	}
	if err := syscall.Kill(1, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal server PID 1: %w", err)
	}
	time.Sleep(delay)
	return nil
}
