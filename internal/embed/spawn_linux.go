//go:build linux

package embed

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

// Managed components receive SIGTERM when the gateway dies, including on
// SIGKILL where no cleanup code runs. Linux delivers PR_SET_PDEATHSIG when the
// *thread* that forked the child exits, not the process, and the Go runtime may
// retire threads (a goroutine that exits while locked to its thread takes the
// thread with it). Every managed child is therefore forked from one goroutine
// that locks its OS thread and never returns, so that thread lives exactly as
// long as the gateway process.
//
// Process groups are not used: in exec-based CI containers the gateway's own
// Setpgid is unreliable (see restart_e2e_test.go), and the components do not
// spawn grandchildren.

type spawnRequest struct {
	cmd  *exec.Cmd
	done chan error
}

var (
	spawnOnce     sync.Once
	spawnRequests chan spawnRequest
)

func startPlatformCommand(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
	spawnOnce.Do(func() {
		spawnRequests = make(chan spawnRequest)
		go spawnLoop(spawnRequests)
	})
	done := make(chan error, 1)
	spawnRequests <- spawnRequest{cmd: cmd, done: done}
	return <-done
}

func spawnLoop(requests <-chan spawnRequest) {
	runtime.LockOSThread()
	// Never UnlockOSThread and never return: the thread must outlive every
	// child forked from it.
	for request := range requests {
		request.done <- request.cmd.Start()
	}
}
