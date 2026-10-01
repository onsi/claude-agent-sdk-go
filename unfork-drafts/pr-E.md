# feat: Add typed task lifecycle messages and StopTask (Issue #143)

## Summary

The CLI reports subagents and background commands as tasks and can stop one of them by ID, but the SDK parsed the task messages as plain `SystemMessage`s and had no way to send `stop_task`. @onsi reported this in #143. This PR follows Python (README rows #10 and #11, post-snapshot P27), expressed in idiomatic Go.

| Part | Python | Go (this PR) |
|---|---|---|
| Typed task messages | `TaskStartedMessage`, `TaskProgressMessage`, `TaskNotificationMessage`, `TaskUpdatedMessage` subclass `SystemMessage` and keep `subtype`/`data` | Same four structs, each embedding `SystemMessage` (so `Subtype` and the raw `Data` stay available). The stream still delivers `*SystemMessage`; `AsTaskStarted()`, `AsTaskProgress()`, `AsTaskNotification()` and `AsTaskUpdated()` return the typed form. |
| Field set | required: plain fields; optional: `X \| None = None` | Same JSON names. Required fields are values, optional ones pointers. `TaskUpdatedMessage.Status` is `patch["status"]`. |
| Statuses | `TaskNotificationStatus`, `TaskUpdatedStatus` literals; `TERMINAL_TASK_STATUSES` frozenset | `TaskNotificationStatus` and `TaskUpdatedStatus` string types with constants; `IsTerminalTaskStatus(status string) bool` (completed, failed, stopped, killed) |
| Malformed `task_started`/`task_progress`/`task_notification` | `MessageParseError` (missing key) | `MessageParseError`: `"task_started message missing task_id field"` |
| Malformed `task_updated` | never raises; patch not a dict becomes `{}`, `task_id` defaults to `""` | Same: never a parse error |
| `stop_task(task_id)` | `{"subtype":"stop_task","task_id":...}` control request; `CLIConnectionError` before `connect()` | `Client.StopTask(ctx, taskID)` → `Transport.StopTask` → `Protocol.StopTask`; `"client not connected"` before `Connect()` |

Public API added:

```go
// Client and Transport
StopTask(ctx context.Context, taskID string) error

// SystemMessage methods
func (m *SystemMessage) AsTaskStarted() (*TaskStartedMessage, bool)
func (m *SystemMessage) AsTaskProgress() (*TaskProgressMessage, bool)
func (m *SystemMessage) AsTaskNotification() (*TaskNotificationMessage, bool)
func (m *SystemMessage) AsTaskUpdated() (*TaskUpdatedMessage, bool)

// Types
type TaskStartedMessage struct {
    SystemMessage
    TaskID, Description, UUID, SessionID string
    ToolUseID, TaskType                  *string
}
type TaskProgressMessage struct {
    SystemMessage
    TaskID, Description     string
    Usage                   TaskUsage
    UUID, SessionID         string
    ToolUseID, LastToolName *string
}
type TaskNotificationMessage struct {
    SystemMessage
    TaskID                                  string
    Status                                  TaskNotificationStatus
    OutputFile, Summary, UUID, SessionID    string
    ToolUseID                               *string
    Usage                                   *TaskUsage
}
type TaskUpdatedMessage struct {
    SystemMessage
    TaskID          string
    Patch           map[string]any
    Status          *TaskUpdatedStatus
    SessionID, UUID *string
}
type TaskUsage struct{ TotalTokens, ToolUses, DurationMs int }
type TaskNotificationStatus string // Completed, Failed, Stopped
type TaskUpdatedStatus string      // Pending, Running, Paused, Completed, Failed, Killed
func IsTerminalTaskStatus(status string) bool

// Constants
SystemSubtypeTaskStarted, SystemSubtypeTaskProgress, SystemSubtypeTaskNotification, SystemSubtypeTaskUpdated
TaskNotificationStatus{Completed,Failed,Stopped}, TaskUpdatedStatus{Pending,Running,Paused,Completed,Failed,Killed}
SubtypeStopTask = "stop_task"; type StopTaskRequest
```

Usage:

```go
switch msg := message.(type) {
case *claudecode.SystemMessage:
    if started, ok := msg.AsTaskStarted(); ok {
        _ = client.StopTask(ctx, started.TaskID)
    }
    if updated, ok := msg.AsTaskUpdated(); ok && updated.Status != nil &&
        claudecode.IsTerminalTaskStatus(string(*updated.Status)) {
        // the task ended
    }
}
```

CLI fields that Python does not model (`subagent_type`, `is_backgrounded`, `spawn_depth`, `prompt`, `workflow_name`, `skip_transcript`, `summary` on `task_progress`, ...) are not promoted to struct fields. They stay in the embedded `Data`, as they do in Python's `data`.

## Python reference

- `src/claude_agent_sdk/types.py:1161-1190`: `TaskUsage`, `TaskNotificationStatus`, `TaskUpdatedStatus`, `TERMINAL_TASK_STATUSES`.
- `src/claude_agent_sdk/types.py:1193-1279`: `TaskStartedMessage`, `TaskProgressMessage`, `TaskNotificationMessage`, `TaskUpdatedMessage`, each `@dataclass class X(SystemMessage)`.
- `src/claude_agent_sdk/_internal/message_parser.py:226-306`: the `system` case. `data["task_id"]` etc. for required fields, `data.get(...)` for optional ones, a `KeyError` becomes `MessageParseError("Missing required field in system message: ...")`. `task_updated` (`:266-289`) reads everything with `.get`, replaces a non-dict `patch` with `{}`, and takes `status` from `patch.get("status")`.
- `src/claude_agent_sdk/client.py:443-469`: `stop_task(task_id)`, `CLIConnectionError("Not connected. Call connect() first.")` before `connect()`.
- `src/claude_agent_sdk/_internal/query.py:857-868`: `_send_control_request({"subtype": "stop_task", "task_id": task_id})`.

## Behavior change

- **Task messages are still delivered as `*SystemMessage`.** Python gets both `case SystemMessage()` and `case TaskStartedMessage()` from one subclass. Go has no subtype relation between concrete types, so a typed message on the stream would have to leave every existing `case *SystemMessage:` arm, and hosts that handle task events there today would stop seeing them with no compile error. This PR keeps the delivered type and adds the typed form behind `AsTask*()`. The cost is one extra step (`case *SystemMessage:` then `AsTaskStarted()`) instead of a direct `case *TaskStartedMessage:`. The typed structs embed `SystemMessage`, implement `Message` through it, and marshal to the raw payload, so nothing is lost either way.
- **A `task_started`, `task_progress` or `task_notification` message that lacks a required field is now a `MessageParseError`** on the error channel instead of a `SystemMessage`, as in Python. The transport keeps reading after it, and `ReceiveResponse`'s `Next` returns it. One difference from Python: Python checks only that the key exists, while Go also requires the right JSON type (a string for string fields, an object for `task_progress.usage`), because the fields are typed. `task_updated` never fails, as in Python.
- **`Transport` gains `StopTask(ctx, taskID)`.** A custom `Transport` must add it, as with `GetMcpStatus` in #124; `return nil` is enough for a test double.

## Tests

| Test | Result on old code |
|---|---|
| `TestParseTaskStartedMessage` (full, minimal, optional fields of the wrong type, missing each of `task_id`/`description`/`uuid`/`session_id`) | `AsTaskStarted() ok = false, want true`; `ParseMessage error = <nil>, want one containing "task_started message missing task_id field"` |
| `TestParseTaskProgressMessage` (full, minimal, missing each of `task_id`/`description`/`usage`/`uuid`/`session_id`) | `AsTaskProgress() ok = false, want true`; `ParseMessage error = <nil>, want one containing "task_progress message missing usage field"` |
| `TestParseTaskNotificationMessage` (full, minimal, missing each of `task_id`/`status`/`output_file`/`summary`/`uuid`/`session_id`) | `AsTaskNotification() ok = false, want true`; `ParseMessage error = <nil>, want one containing "task_notification message missing status field"` |
| `TestParseTaskUpdatedMessage` (full, minimal, patch without status, patch not an object, non-string status, only the subtype) | `AsTaskUpdated() ok = false, want true` |
| `TestParseNonTaskSystemMessageIsNotTyped`, `TestTaskAccessorsRejectOtherSubtypes` | guards: an `init` message with a `task_id` is not a task message (pass on the stub) |
| `TestTaskMessageKeepsSystemMessage` (`Type()` is `"system"`, marshals to the raw payload) | `AsTaskStarted() ok = false` |
| `TestIsTerminalTaskStatus` | guard on the `TERMINAL_TASK_STATUSES` set |
| `TestProtocolStopTask/{success,error_response}` (exact request body on the wire) | `request = , want {"subtype":"stop_task","task_id":"task-abc123"}`; `StopTask error = <nil>, want one containing "no such task"` |
| `TestClientStopTask/{success,not_connected,context_cancelled,transport_error}` (mock transport) | `stopped task IDs = [], want [task-abc123]`; `expected error containing "not connected", got nil` |
| `TestTransportControlProtocolIntegration/StopTask_requires_connection` and `/StopTask_in_streaming_mode_with_protocol` (`TestMain` mock CLI) | `Expected error containing "not connected", got nil` |

"Old code" is this branch with the new methods stubbed (`AsTask*` return `nil, false`, the parser skips validation, the three `StopTask`s return `nil`), since the tests do not compile against main. All tests use the in-package mocks or the `TestMain` mock CLI, so they run on Windows too.

## Gates

- [x] `gofmt -s -l .` clean
- [x] `go vet ./...` and `GOOS=windows go vet ./...` clean
- [x] `golangci-lint run ./...` (v2.4.0): 0 issues
- [x] `gocyclo -over 15 .`: no new entries
- [x] `ginkgo -r --race`: all suites pass

## Checklist

- [x] All tests pass
- [x] No linting errors
- [x] Code follows style guidelines
- [x] Documentation updated: README (Advanced Features), `docs/reference.md` (Client interface, `StopTask()`, Task Messages, Transport interface), `docs/architecture/interfaces.md`, `docs/parity.md`, `docs/tracking/README.md` rows #10 (partial) and #11 (done), `docs/tracking/post-snapshot.md` P27 (done)
- [x] Commit messages follow conventions

## What remains of #143

#143 proposed three changes:

1. Route `Interrupt` through the control protocol: done in #154.
2. Typed task messages: done here, Python-shaped. `subagent_type`, `is_backgrounded` and the spawn depth are in `Data`, not typed fields, because Python does not model them.
3. `StopTask(ctx, taskID)`: done here. `BackgroundTasks(ctx, toolUseID)` (the `background_tasks` control request) is not included: it exists only in the TypeScript SDK and Python has no equivalent. It could be a follow-up if you want Go to go beyond Python there.

Row #10 stays partial: `ReconnectMcpServer` and `ToggleMcpServer` are still pending.

Closes #143
