Title: feat: Add Client.Done and Client.Err to report CLI process exit (Issue #144)

Branch: `feature/issue-144-client-done-err`

## Summary

#156 fixed #144 for the `ReceiveResponse` and `Query` iterators. A host that reads the `ReceiveMessages()` channel still cannot tell that the CLI process is gone. That is the path the README's streaming example uses. Measured on main (c3bc12b) with a stub CLI:

1. After the CLI is SIGKILLed, the `ReceiveMessages` channel closes with no reason. The `*ProcessError` goes to the transport error channel, which only `ReceiveResponse` reads.
2. `QueryStream` on the dead client returns `nil`. Its write error goes to `streamErrChan`, which only `ReceiveResponse` reads.
3. `Interrupt`, `SetModel` and `GetMcpStatus` return an untyped `failed to send control request: write |1: file already closed`.
4. A CLI that exits while a descendant still holds its stdout is reaped at once, but `ReceiveMessages` stays open until the descendant exits (8 s in the probe). Nothing tells the host that the CLI is gone.

Python's `receive_messages()` raises the `ProcessError`. A Go `<-chan Message` cannot carry an error, so this PR uses the `context.Context` pattern:

| Part | Python | Go (this PR) |
|---|---|---|
| Exit is visible on the streaming path | `receive_messages()` re-raises the `{"type": "error", "exception": ProcessError}` that `_read_messages` puts in the stream | `Client.Done()` closes when the CLI process exits, on its own or through `Disconnect`. It closes on process exit, not on stdout EOF. |
| Why it exited | `ProcessError(exit_code)` | `Client.Err()`: `*ProcessError` for a non-zero exit (`ExitCode` -1 for a signal), `*ConnectionError` for a clean exit, `"client not connected"` before `Connect` and after `Disconnect`, nil while running |
| Write after exit | `CLIConnectionError("Cannot write to process that exited with error: ...") from self._exit_error` | `Query`, `QueryWithSession`, `QueryStream`, `Interrupt`, `SetModel`, `SetPermissionMode`, `RewindFiles`, `GetMcpStatus` return `*ConnectionError("cannot write to terminated CLI process", Err())`. `errors.As` finds both the `*ConnectionError` and the `*ProcessError`. `SendMessage` in the transport uses the same error. |

### Design

- `subprocess.Transport` gets `Done()` and `Err()`. They read the existing `processDone` waiter (#150) and `cmd.ProcessState`. There is no second `Wait`. `childProcess.exitError` and `Err` share `processExitError`, so the message matches the one #156 sends on the error channel.
- `Done`/`Err` are not added to the public `Transport` interface, because that would break custom transports. `ClientImpl` type-asserts an unexported `processWatcher` interface. The `Transport` doc comment describes the two methods. With a custom transport that lacks them, `Client.Done()` closes on `Disconnect` and `Err()` is nil while connected.
- `Done` and `Err` describe the current connection. `Connect` makes a new `disconnected` channel, and the default path builds a new `subprocess.Transport` on each `Connect`. A channel from an earlier connection stays closed.
- Clean exit: `Err()` is non-nil whenever `Done()` is closed, as with `context.Context`. A CLI that exits 0 while the client is connected cannot serve another turn. Python's write raises `CLIConnectionError("Cannot write to terminated process (exit code: 0)")` in that case, so Go returns a `*ConnectionError` and not a `*ProcessError`. `AsProcessError(client.Err())` stays a "the CLI failed" check.
- After `Disconnect`, `Err()` returns the same `"client not connected"` error every method already returns. It does not report the SIGTERM from our own shutdown as a crash.

## Python reference

- `src/claude_agent_sdk/_internal/query.py:547-554`: the read loop puts `{"type": "error", "error": ..., "exception": pending_error}` in the stream.
- `src/claude_agent_sdk/_internal/query.py:1135-1145`: `receive_messages()` re-raises that exception.
- `src/claude_agent_sdk/_internal/transport/subprocess_cli.py:1063-1087`: `write()` raises `CLIConnectionError` for a terminated process, `from self._exit_error` after a failed exit.
- `src/claude_agent_sdk/_internal/transport/subprocess_cli.py:1150-1162`: a non-zero exit becomes `ProcessError(exit_code=returncode)`.

## Behavior change

- `Client` gains `Done() <-chan struct{}` and `Err() error`. This breaks any type outside the SDK that implements `Client`, for example a hand-written mock. `NewClient` and `NewClientWithTransport` return the SDK's own type, so code that only calls them is not affected.
- `QueryStream` on a client whose CLI has exited returns an error instead of `nil`.
- `Interrupt`, `SetModel`, `SetPermissionMode`, `RewindFiles` and `GetMcpStatus` on a dead client return `*ConnectionError` instead of a raw pipe error.
- `Query` already returned a `*ConnectionError` on a dead client. Its message changes from `cannot write to terminated CLI process (signal: killed)` to `cannot write to terminated CLI process: Claude Code process exited unexpectedly (signal: killed) (exit code: -1)` plus the `Error output` line. It now wraps the `*ProcessError`.
- The README streaming example returns `client.Err()` when the message channel closes, instead of `nil`.

Known limitation (main has it too): with `WithDebugWriter` set to a writer that is not an `*os.File`, `os/exec` copies stderr on a goroutine, and `cmd.Wait` waits for that copy. A descendant that inherits stderr therefore delays `processDone`, and so `Done`. With a stub whose descendant keeps stderr for 4 s, `Done` closed after 0 s without a DebugWriter and after 4 s with a `bytes.Buffer` DebugWriter. Routing the DebugWriter through `os.Pipe` (like the stderr callback) would fix it. That is out of scope here.

## Tests

The "old code" column for the new API comes from a run against compile stubs: `Done()` returned a channel that never closes and `Err()` returned nil, which matches main, where nothing reports the exit.

| Test | Old code |
|---|---|
| `TestTransportDoneReportsCLIExit/non_zero_exit` (mock `exit_nonzero`), `/killed`, `/clean_exit` (new mock `exit_clean`) | `Done() did not close after the CLI exited` |
| `TestTransportDoneClosesBeforeStdoutEOF` (new mock `orphan_stdout`: re-runs the test binary as a `hold_stdout` descendant that inherits stdout for 4 s, then exits 3; works on Windows) | `message channel closed before Done(), want Done() first` (the channel closed 4 s after the CLI exited) |
| `TestTransportDoneBeforeConnectAndAfterClose` | `Done() is open before Connect, want closed` |
| `TestClientCallsFailAfterProcessExit` (table over the 8 methods, `processMockTransport`) | `Done() after the CLI exited is open, want closed`. With the Done/Err checks removed: `QueryStream on a dead client = <nil>, want a *ConnectionError wrapping the exit error`, and the same for the other 7 methods. The mock does not model the closed pipe; against a real CLI, main already rejects `Query` with a `*ConnectionError`. |
| `TestClientDoneAndErrLifecycle/process_transport`, `/custom_transport_fallback` (before Connect, connected, after Disconnect, reconnect) | `Done() before Connect is open, want closed` |
| `TestSubprocessTransportReportsProcessExit` | guard: fails if `subprocess.Transport` stops satisfying `processWatcher`, which would silently drop to the Disconnect-only fallback |

Probe (stub CLI, `SIGKILL` of the child):

| Check | main | this PR |
|---|---|---|
| exit visible on the `ReceiveMessages` path | channel closes, no reason | `Done()` closes at once; `Err()` = `*ProcessError`, `ExitCode` -1 |
| `QueryStream` on the dead client | `<nil>` | `*ConnectionError` wrapping the `*ProcessError` |
| `Interrupt` / `SetModel` on the dead client | `write \|1: file already closed` (untyped) | `*ConnectionError` wrapping the `*ProcessError` |
| CLI exits 0, descendant holds stdout 8 s | nothing until the channel closes at 8 s | `Done()` at once; `Err()` = `Claude Code process exited (exit status 0)`; channel still closes at 8 s |
| `Disconnect()` then `Done()`/`Err()` | - | closed / `client not connected` |

## Test plan

- [x] All tests pass: `ginkgo -r --race` (runs the `testing` tests in all 7 packages, including the fuzz seed corpora in `internal/parser` and `internal/shared`, which is what `make fuzz-test` runs): `Ginkgo ran 7 suites in 3m0s`, `Test Suite Passed`.
- [x] No linting errors: `golangci-lint run ./...` (v2.4.0): 0 issues.
- [x] Code follows style guidelines: `gofmt -s -l .` clean; `go vet ./...` and `GOOS=windows go vet ./...` clean; `gocyclo -over 15 .` shows no new entries.
- [x] Documentation updated: `README.md` (streaming example, feature list), `docs/reference.md`, `docs/architecture/interfaces.md`, `docs/parity.md`, `CLAUDE.md`, `internal/subprocess/CLAUDE.md`. There is no tracker row: Python has no equivalent API.
- [x] Commit messages follow conventions.

Related: #144 (closed by #156; this is the `ReceiveMessages` half).
