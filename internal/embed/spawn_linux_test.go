//go:build linux

package embed

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestManagedChildDiesWithParent proves a managed component cannot outlive a
// gateway that is SIGKILLed: no cleanup code runs in that case, so only the
// kernel's parent-death signal can stop the child from keeping its port.
func TestManagedChildDiesWithParent(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	parent := exec.Command(os.Args[0], "-test.run=^TestSpawnHelperProcess$")
	parent.Env = append(os.Environ(), "LO_SPAWN_HELPER=parent", "LO_SPAWN_PID_FILE="+pidFile)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for childPID == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		_ = parent.Process.Kill()
		t.Fatal("helper parent never reported its child")
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processExited(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("managed child %d survived its SIGKILLed parent", childPID)
}

// processExited treats a zombie as exited: PID 1 in a container may not reap
// orphans promptly, but a zombie no longer holds sockets.
func processExited(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}

func TestSpawnHelperProcess(t *testing.T) {
	switch os.Getenv("LO_SPAWN_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestSpawnHelperProcess$")
		child.Env = append(os.Environ(), "LO_SPAWN_HELPER=child")
		if err := startManagedCommand(child); err != nil {
			os.Exit(3)
		}
		pidFile := os.Getenv("LO_SPAWN_PID_FILE")
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(time.Hour)
	case "child":
		time.Sleep(time.Hour)
	}
}
