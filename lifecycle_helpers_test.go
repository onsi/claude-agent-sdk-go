//go:build !windows

package claudecode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeLifecycleCLI writes a stub claude that records its pid, then runs body.
func writeLifecycleCLI(t *testing.T, body string) (cliPath, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "pid")
	cliPath = filepath.Join(dir, "claude")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then echo 3.0.0; exit 0; fi\necho $$ > %q\n%s\n", pidFile, body)
	if err := os.WriteFile(cliPath, []byte(script), 0o700); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatalf("write stub CLI: %v", err)
	}
	return cliPath, pidFile
}

func readLifecyclePID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile) // #nosec G304 -- path is under t.TempDir
		if err == nil && strings.HasSuffix(string(data), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("parse stub pid: %v", err)
			}
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stub CLI never wrote %s", pidFile)
	return 0
}

// assertReaped fails if pid is still our unreaped child, running or zombie.
func assertReaped(t *testing.T, pid int) {
	t.Helper()
	var status syscall.WaitStatus
	got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if errors.Is(err, syscall.ECHILD) {
		return
	}
	switch {
	case err != nil:
		t.Errorf("wait4(%d): %v", pid, err)
	case got == 0:
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child %d is still running", pid)
	default:
		t.Errorf("child %d exited but was never reaped", pid)
	}
}
