//go:build !windows

package subprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/severity1/claude-agent-sdk-go/internal/control"
	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

func TestConnectHandshakeFailureReapsChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	cliPath := filepath.Join(dir, "claude")
	result := `{"type":"result","subtype":"error_during_execution","duration_ms":1,"duration_api_ms":1,"is_error":true,"num_turns":0,"session_id":"s1","errors":["no such session"]}`
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then echo 3.0.0; exit 0; fi\necho $$ > %q\nread -r _\necho '%s'\nexec cat >/dev/null\n", pidFile, result)
	if err := os.WriteFile(cliPath, []byte(script), 0o700); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatalf("write stub CLI: %v", err)
	}
	transport := New(cliPath, &shared.Options{
		Hooks: map[control.HookEvent][]control.HookMatcher{control.HookEventPreToolUse: nil},
	}, false, "sdk-go-client")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := transport.Connect(ctx)
	if err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("Connect = %v, want the CLI's initialization error", err)
	}

	data, readErr := os.ReadFile(pidFile) // #nosec G304 -- path is under t.TempDir
	if readErr != nil {
		t.Fatalf("read pid: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil {
		t.Fatalf("parse pid: %v", convErr)
	}
	var status syscall.WaitStatus
	got, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if !errors.Is(waitErr, syscall.ECHILD) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("child %d not reaped after failed Connect (wait4 = %d, %v)", pid, got, waitErr)
	}
	select {
	case <-transport.Done():
	default:
		t.Error("Done is open after a failed Connect")
	}
	if transport.IsConnected() {
		t.Error("transport reports connected after a failed Connect")
	}
}
