//go:build !linux

package embed

import "os/exec"

func startPlatformCommand(cmd *exec.Cmd) error { return cmd.Start() }
