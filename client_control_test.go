package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// controlStubCLI is a streaming-mode `claude` stand-in. It appends every stdin
// line to log, answers every control request with success, and records
// "SIGINT" in log if it is ever sent SIGINT. A user message containing "spawn"
// starts task-1; stop_task finishes it as stopped; interrupt ends the running
// turn with a result; any other user message is answered in full.
const controlStubCLI = `#!/bin/bash
if [ "$1" = "-v" ]; then echo "3.0.0"; exit 0; fi
log=%q
trap 'echo SIGINT >> "$log"; exit 130' INT
result='{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"num_turns":1,"session_id":"s1"}'
while IFS= read -r line; do
  printf '%%s\n' "$line" >> "$log"
  case "$line" in
    *'"type":"control_request"'*)
      req_id=$(printf '%%s' "$line" | grep -o '"request_id":"[^"]*"' | cut -d'"' -f4)
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$req_id"
      case "$line" in
        *'"subtype":"stop_task"'*)
          printf '%%s\n' '{"type":"system","subtype":"task_notification","task_id":"task-1","tool_use_id":"toolu_1","status":"stopped","output_file":"","summary":"stopped","uuid":"u2","session_id":"s1"}' ;;
        *'"subtype":"interrupt"'*)
          printf '%%s\n' "$result" ;;
      esac ;;
    *'"type":"user"'*spawn*)
      printf '%%s\n' '{"type":"system","subtype":"task_started","task_id":"task-1","tool_use_id":"toolu_1","description":"research","subagent_type":"general-purpose","is_backgrounded":false,"spawn_depth":1,"uuid":"u1","session_id":"s1"}' ;;
    *'"type":"user"'*long*)
      printf '%%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}],"model":"stub"}}' ;;
    *'"type":"user"'*)
      printf '%%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answered"}],"model":"stub"}}'
      printf '%%s\n' "$result" ;;
  esac
done
`

const windowsGOOS = "windows"

func connectControlStubClient(ctx context.Context, t *testing.T) (Client, <-chan Message, string) {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		t.Skip("stub CLI is a bash script")
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "stdin.log")
	cliPath := filepath.Join(dir, "claude")
	script := fmt.Sprintf(controlStubCLI, logPath)
	err := os.WriteFile(cliPath, []byte(script), 0o755) // #nosec G306 - test stub must be executable
	if err != nil {
		t.Fatalf("write stub CLI: %v", err)
	}

	client := NewClient(WithCLIPath(cliPath))
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	return client, client.ReceiveMessages(ctx), logPath
}

func awaitMessage[T Message](t *testing.T, msgs <-chan Message) T {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				var zero T
				t.Fatalf("message channel closed while waiting for %T: the CLI process is gone", zero)
			}
			if typed, ok := msg.(T); ok {
				return typed
			}
		case <-timeout:
			var zero T
			t.Fatalf("timed out waiting for %T", zero)
		}
	}
}

func stubStdinLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath) // #nosec G304 -- path is inside t.TempDir()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read stub log: %v", err)
	}
	return string(data)
}

func TestClientInterruptKeepsSessionAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, msgs, logPath := connectControlStubClient(ctx, t)

	if err := client.Query(ctx, "start a long turn"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	awaitMessage[*AssistantMessage](t, msgs)

	if err := client.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	awaitMessage[*ResultMessage](t, msgs)

	log := stubStdinLog(t, logPath)
	if strings.Contains(log, "SIGINT") {
		t.Fatalf("Interrupt signalled the CLI process; stdin log:\n%s", log)
	}
	if !strings.Contains(log, `"request":{"subtype":"interrupt"}`) {
		t.Fatalf("no in-band interrupt control request was written; stdin log:\n%s", log)
	}

	if err := client.Query(ctx, "follow-up"); err != nil {
		t.Fatalf("follow-up Query after Interrupt: %v", err)
	}
	answer := awaitMessage[*AssistantMessage](t, msgs)
	if text, ok := answer.Content[0].(*TextBlock); !ok || text.Text != "answered" {
		t.Errorf("follow-up answer = %#v, want the stub's reply", answer.Content)
	}
	awaitMessage[*ResultMessage](t, msgs)

	if log := stubStdinLog(t, logPath); strings.Contains(log, "SIGINT") {
		t.Errorf("CLI process received SIGINT; stdin log:\n%s", log)
	}
}

func TestClientStopTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, msgs, logPath := connectControlStubClient(ctx, t)

	if err := client.Query(ctx, "spawn a subagent"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	started := awaitMessage[*TaskStartedMessage](t, msgs)
	if started.TaskID != "task-1" || started.SubagentType == nil || *started.SubagentType != "general-purpose" {
		t.Fatalf("unexpected TaskStartedMessage: %+v", started)
	}

	if err := client.StopTask(ctx, started.TaskID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	notification := awaitMessage[*TaskNotificationMessage](t, msgs)
	if notification.TaskID != "task-1" || notification.Status != TaskNotificationStatusStopped {
		t.Errorf("unexpected TaskNotificationMessage: %+v", notification)
	}

	log := stubStdinLog(t, logPath)
	if !strings.Contains(log, `"request":{"subtype":"stop_task","task_id":"task-1"}`) {
		t.Errorf("no stop_task control request was written; stdin log:\n%s", log)
	}
}

func TestClientBackgroundTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, _, logPath := connectControlStubClient(ctx, t)

	backgrounded, err := client.BackgroundTasks(ctx, "toolu_1")
	if err != nil {
		t.Fatalf("BackgroundTasks: %v", err)
	}
	if !backgrounded {
		t.Error("BackgroundTasks = false, want true for a success response without the field")
	}
	if _, err := client.BackgroundTasks(ctx, ""); err != nil {
		t.Fatalf("BackgroundTasks(all): %v", err)
	}

	log := stubStdinLog(t, logPath)
	for _, want := range []string{
		`"request":{"subtype":"background_tasks","tool_use_id":"toolu_1"}`,
		`"request":{"subtype":"background_tasks"}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("stdin log missing %s:\n%s", want, log)
		}
	}
}

func TestClientTaskMethodsDelegateToTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transport := newClientMockTransport()
	transport.backgrounded = true
	client := setupClientForTest(t, transport)
	defer disconnectClientSafely(t, client)
	connectClientSafely(ctx, t, client)

	if err := client.StopTask(ctx, "task-9"); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	got, err := client.BackgroundTasks(ctx, "toolu_9")
	if err != nil || !got {
		t.Fatalf("BackgroundTasks = %v, %v", got, err)
	}
	if len(transport.stoppedTaskIDs) != 1 || transport.stoppedTaskIDs[0] != "task-9" {
		t.Errorf("stoppedTaskIDs = %v", transport.stoppedTaskIDs)
	}
	if len(transport.backgroundToolUseIDs) != 1 || transport.backgroundToolUseIDs[0] != "toolu_9" {
		t.Errorf("backgroundToolUseIDs = %v", transport.backgroundToolUseIDs)
	}

	transport.taskError = errors.New("task control failed")
	if err := client.StopTask(ctx, "task-9"); err == nil || !strings.Contains(err.Error(), "task control failed") {
		t.Errorf("StopTask error = %v", err)
	}
	if _, err := client.BackgroundTasks(ctx, ""); err == nil {
		t.Error("BackgroundTasks should propagate the transport error")
	}
}

func TestClientTaskMethodsRequireConnection(t *testing.T) {
	ctx := context.Background()
	client := NewClientWithTransport(newClientMockTransport())

	if err := client.StopTask(ctx, "task-1"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("StopTask before Connect: %v", err)
	}
	if _, err := client.BackgroundTasks(ctx, ""); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("BackgroundTasks before Connect: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := client.StopTask(cancelled, "task-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("StopTask with cancelled context: %v", err)
	}
}
