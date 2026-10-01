# fix: fail pending control requests when the CLI exits or the protocol closes

## Summary

A control request waits for its response, its timeout (5 s for `Interrupt`, `SetModel`, `SetPermissionMode`, `GetMcpStatus`, `RewindFiles`), or an init error. Two events left it waiting for the whole timeout even though no answer could come any more:

| | Cause | Python | This PR |
|---|---|---|---|
| A. The CLI's stream ended with an error (non-zero exit, stdout scanner error) | `handleStdout` sent the error to `errChan` only | `_read_messages` sets the reader's error on every pending request | `Protocol.FailPendingRequests(err)`, called from `handleStdout`: every waiting request returns that error at once |
| B. Our own `Close()` ran | `Close` did not wake waiting requests, and a request after `Close` was still written | no equivalent (see below) | `ErrProtocolClosed`: `Close` wakes every waiting request, and a request after `Close` fails without being written |

There are two commits: the first is the Go lifecycle fix (B), the second the parity fix (A).

## Python reference (A)

`src/claude_agent_sdk/_internal/query.py:539-546`:

```python
# Signal all pending control requests so they fail fast instead of
# timing out. ...
for request_id, event in list(self.pending_control_responses.items()):
    if request_id not in self.pending_control_results:
        self.pending_control_results[request_id] = pending_error
        event.set()
```

It runs in `_read_messages`'s `except Exception`, i.e. when the reader ends with an error such as the `ProcessError` for a non-zero exit. The error is the same object the stream raises (`query.py:551-554`, `receive_messages` at `query.py:1141-1145`).

Python has nothing for B. `close()` cancels the read task, and `_read_messages` re-raises `CancelledError` (`query.py:508-511`) before it reaches that loop. B is a Go-only guarantee, kept as a separate sentinel so callers can tell "the client shut down" from "the CLI died".

## Fix

A, `internal/control` and `internal/subprocess/io.go`:

- `Protocol.FailPendingRequests(err)` hands `err` to every request waiting in `SendControlRequest`, through its response channel (an unexported `Response.failure`).
- `handleStdout` calls it, through `endStreamWithError`, for the two terminal errors: the exit `*ProcessError` and `stdout scanner error`. It runs before the error is sent to `errChan`. A parse error does not end the Go reader, so it does not call it.
- It does nothing until the initialize handshake has completed. A request waiting for the handshake is already failed by `HandleControlInitErr`: the stdout-closed watcher and `routeInitError` feed it, and `TestTransportConnectFailureReapsProcess` pins that path. Failing it a second way would race the two messages. Python does cover `initialize` in this loop; Go does not need to, for that reason.
- The error is not sticky. Python does not fail a request sent after the reader ended either, and in Go that write fails because the process is gone.

B, `internal/control/protocol.go`:

- New `ErrProtocolClosed`. `Close` closes a `closedCh`, which `SendControlRequest` selects on beside its response, `initErrChan` and timeout cases.
- `SendControlRequest` checks `p.closed` under `p.mu`, in the critical section that registers the request, so nothing registers after `Close`.
- `Initialize` wraps it as before, so `errors.Is` works. `ErrProtocolClosed` is in an `internal` package; callers see the message `control protocol closed`.

## Measured (stub CLI, `Client.Interrupt`)

| Scenario | Before | After |
|---|---|---|
| CLI exits 1 on receiving `interrupt` | `control request timeout: context deadline exceeded` after 5 s | `Claude Code process exited unexpectedly (exit status 1) (exit code: 1)` after 0 s |
| CLI never answers `interrupt`; `Disconnect` 200 ms later | `control request timeout` after 5 s | `control protocol closed` after 200 ms |

## Behavior change

- A control request waiting when the CLI exits non-zero, or when the stdout scanner fails, returns that `*ProcessError` or scanner error. Before, it returned `control request timeout`.
- A control request waiting when `Close` runs returns `control protocol closed` instead of `control request timeout`.
- A control request sent after `Close` fails at once and is not written.

## Tests

| Test | Result on old code |
|---|---|
| A: `TestFailPendingRequestsWakesWaiters/{one,several}_pending_request(s)` | `request 1 of 1 still pending after FailPendingRequests` (method stubbed for the old run) |
| A: `TestFailPendingRequestsLeavesInitializeToInitErr` | pins the init gate (passes on the stub; it guards against double-failing initialize) |
| A: `TestStreamEndFailsPendingInterrupt/cli_exits_non_zero` (mock `exit_on_interrupt`) | `Interrupt still pending 2.001425167s after the stream ended with an error` |
| A: `TestStreamEndFailsPendingInterrupt/stdout_scanner_error` (mock `overflow_on_interrupt`, `MaxBufferSize` 1024) | `Interrupt still pending 2.00239875s after the stream ended with an error` |
| B: `TestCloseFailsPendingControlRequests/{one,several}_pending_request(s)` | `request 1 of 1 still pending 1.002503125s after Close` (it returns at 5 s with a timeout error) |
| B: `TestControlRequestAfterCloseFails/{interrupt,set_model,get_mcp_status,initialize}` | `request after Close error = control request timeout: context deadline exceeded, want ErrProtocolClosed`, 5 s each (30 s for `initialize`), and `wrote 1 request(s) after Close` |
| B: `TestCloseFailsPendingInterrupt` (real `Transport`, mock `ignore_interrupt`) | `Interrupt still pending 2s after Close` |

All of them use the `TestMain` mock CLI or the in-package mock transport, so they run on Windows too. The `ignore_interrupt` test waits for an `INTERRUPT` event from the mock before it closes, so the request is known to be pending.

## Gates

- [x] `gofmt -s -l .` clean
- [x] `go vet ./...` and `GOOS=windows go vet ./...` clean
- [x] `golangci-lint run ./...` (v2.4.0): 0 issues
- [x] `gocyclo -over 15 .`: no new entries
- [x] `make fuzz-test` passes
- [x] `ginkgo --no-color -r --race`: all 7 suites pass
- [x] New control tests pass `-count=300` under `-race`; new subprocess tests pass `-count=10`
- [x] CLAUDE.md, `internal/control/CLAUDE.md`, `internal/subprocess/CLAUDE.md` updated
