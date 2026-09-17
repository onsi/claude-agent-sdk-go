//go:build !windows

package claudecode

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	lifecycleAssistantLine = `{"type":"assistant","message":{"model":"claude-sonnet-4-5","content":[{"type":"text","text":"hello"}]}}`
	lifecycleResultLine    = `{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"num_turns":1,"session_id":"s1"}`
	lifecycleAssistantMsgs = 200
)

func transcriptBody(exitCode int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "i=0\nwhile [ $i -lt %d ]; do\n  echo '%s'\n  i=$((i+1))\ndone\n", lifecycleAssistantMsgs, lifecycleAssistantLine)
	fmt.Fprintf(&b, "echo '%s'\nexit %d", lifecycleResultLine, exitCode)
	return b.String()
}

func stderrTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "claude_stderr_*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

func drainQuery(t *testing.T, iter MessageIterator) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	count := 0
	for {
		msg, err := iter.Next(ctx)
		if err != nil {
			return count, err
		}
		if msg == nil {
			t.Error("Next returned a nil message with a nil error")
			continue
		}
		count++
	}
}

func TestQueryDrainedIteratorReleasesProcessAndTempFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	tests := []struct {
		name     string
		exitCode int
		wantErr  func(error) bool
	}{
		{
			name:     "clean exit ends with ErrNoMoreMessages",
			exitCode: 0,
			wantErr:  func(err error) bool { return errors.Is(err, ErrNoMoreMessages) },
		},
		{
			name:     "non-zero exit ends with a ProcessError",
			exitCode: 3,
			wantErr: func(err error) bool {
				procErr := AsProcessError(err)
				return procErr != nil && procErr.ExitCode == 3
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cliPath, pidFile := writeLifecycleCLI(t, transcriptBody(tc.exitCode))

			iter, err := Query(context.Background(), "hello", WithCLIPath(cliPath))
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			count, err := drainQuery(t, iter)
			pid := readLifecyclePID(t, pidFile)

			if !tc.wantErr(err) {
				t.Errorf("drain ended with %v", err)
			}
			if want := lifecycleAssistantMsgs + 1; count != want {
				t.Errorf("received %d messages, want %d", count, want)
			}
			assertReaped(t, pid)
			if leaked := stderrTempFiles(t, tmp); len(leaked) != 0 {
				t.Errorf("temp files left after drain: %v", leaked)
			}
			if _, err := iter.Next(context.Background()); !errors.Is(err, ErrNoMoreMessages) {
				t.Errorf("Next after drain = %v, want ErrNoMoreMessages", err)
			}
			if err := iter.Close(); err != nil {
				t.Errorf("Close after drain: %v", err)
			}
		})
	}
}

// The child finishes writing and exits while the consumer is not reading;
// output still buffered in the pipe must survive the child being reaped.
func TestQuerySlowConsumerReceivesWholeTranscript(t *testing.T) {
	cliPath, pidFile := writeLifecycleCLI(t, transcriptBody(0))
	iter, err := Query(context.Background(), "hello", WithCLIPath(cliPath))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = iter.Close() }()
	if _, err := iter.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	pid := readLifecyclePID(t, pidFile)
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	count, err := drainQuery(t, iter)
	if !errors.Is(err, ErrNoMoreMessages) {
		t.Errorf("drain ended with %v", err)
	}
	if want := lifecycleAssistantMsgs; count != want {
		t.Errorf("received %d messages after the first, want %d", count, want)
	}
}
