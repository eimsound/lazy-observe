package embed

import "os/exec"

// startManagedCommand starts a managed component process. Platform files decide
// how the child is tied to the gateway's lifetime.
func startManagedCommand(cmd *exec.Cmd) error {
	return startPlatformCommand(cmd)
}
