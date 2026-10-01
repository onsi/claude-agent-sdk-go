# fix: close does not wait for a descendant that holds the CLI's stdout

## Summary

When the CLI has already exited but a descendant it spawned still holds its stdout (an MCP server, a background shell started by the CLI), `Close()` and `Disconnect()` take the full 5 seconds. The descendant keeps the stdout pipe open, so `handleStdout` never sees EOF and stays blocked in its read. `teardownLocked` waits on `t.wg` for `terminationTimeoutSeconds` before `cleanup()` closes the read end.

Measured with a stub CLI that exits at once and leaves a descendant holding its stdout (Connect, `Query`, sleep 500 ms, `Disconnect`):

| | Before | After |
|---|---|---|
| `Disconnect()` | 5 s | 0 s |

## Python reference

`SubprocessCLITransport.close()` (`src/claude_agent_sdk/_internal/transport/subprocess_cli.py:962-1060`) never waits for stdout EOF. It cancels the stderr reader task first (`:997`), closes the stdin and stderr streams (`:1013`, `:1021`), waits for the process itself to exit, with the 5 s / SIGTERM / 5 s / SIGKILL steps (`:1030-1046`), and drops its stdout stream reference (`:1058`). A descendant holding stdout does not delay it.

## Fix

In `teardownLocked`, once `terminateProcess` has returned (the CLI is reaped) and the context is cancelled:

- Close the stdout read end before waiting for the reader goroutines. The EOF grace from #163 is untouched: stdout still drains while the CLI is alive and `terminateProcess` runs. After the CLI is reaped, messages are dropped anyway while closing (`processStdoutLine` drops them once `closing` is closed), so closing the read end loses nothing a reader would deliver. `handleStdout` returns on the read error; its terminal-error paths already drop the error while `closing` is closed.
- Wait for the readers for `pipeDrainGrace` (500 ms) instead of 5 s. After the stdout close the only reader left is the stderr callback, whose pipe a descendant may also hold. The callback keeps a short chance to deliver the lines already in its pipe (a CLI that dies during `Connect` writes its reason to stderr), then `cleanup` closes the pipe, as it did after 5 s before. In the normal case the readers are already done and the wait returns at once.

No change when nothing holds the pipes: the readers see EOF and return before either step matters.

## Behavior change

- `Close`/`Disconnect` with a descendant holding the CLI's stdout returns right after the CLI is reaped, not 5 s later.
- With `WithStderrCallback` and a descendant that also holds stderr, `Close` returns after at most 500 ms, not 5 s. Lines the callback has not read by then are dropped. Before, the same lines were dropped after 5 s.
- Nothing else changes: the grace after stdin EOF and the SIGTERM and SIGKILL timers are as in #163.

## Tests

`TestCloseDoesNotWaitForDescendantHoldingOutput` uses the `TestMain` mock CLI, so it runs on Windows too. The mock modes `orphan_stdout` and `orphan_stdout_and_stderr` answer initialize, then re-exec the test binary (mode `orphan_holder`) as a descendant with the CLI's stdout (and stderr) inherited, and exit at once. The descendant sleeps 15 s and writes its pid to a file so the test kills it on cleanup. The test waits for `processDone`, then times `Close`.

| Case | Old code | New code |
|---|---|---|
| `descendant_holds_stdout` | `Close took 5.002483834s with a descendant holding the output pipes, want under 3s` | 2.05 s for the whole test (the Connect handshake); Close returns at once |
| `descendant_holds_stdout_and_stderr_with_callback` | `Close took 5.003293167s ...` | 2.54 s for the whole test; Close within the 500 ms grace |

## Not verified

I could not run this on Windows. `GOOS=windows go vet ./...` is clean. On Windows, closing a pipe read end while another goroutine is blocked reading it is the same operation `cleanup` already did after the 5 s wait; this PR only does it earlier.

## Gates

- [x] `gofmt -s -l .` clean
- [x] `go vet ./...` and `GOOS=windows go vet ./...` clean
- [x] `golangci-lint run ./...` (v2.4.0): 0 issues
- [x] `gocyclo -over 15 .`: no new entries
- [x] `make fuzz-test` passes
- [x] `ginkgo --no-color -r --race`: all 7 suites pass
- [x] CLAUDE.md updated
