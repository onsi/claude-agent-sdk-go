## Summary

A CLI that exits non-zero after its turn (for example after an error result) can lose the turn's last messages, or its exit error. `handleStdout` queues every message in `msgChan` (capacity 10), sends the `*ProcessError` to `errChan`, then closes `errChan` and `msgChan`. `queryIterator.Next` and `clientIterator.Next` selected on both channels, and `select` picks at random among ready cases. This follows #156 and #159.

| Case | Old behavior | Python |
|---|---|---|
| Error ready while messages are still buffered | `Next` can return the `*ProcessError` and close, dropping the buffered tail, including the `ResultMessage` | `_read_messages` puts the error in the stream after the last message (`query.py:551-554`), and `receive_messages` raises it only when it reaches it (`query.py:1141-1145`) |
| `msgChan` seen closed while the error is still in the closed `errChan` | `Next` returns `ErrNoMoreMessages` and the exit error is lost | the error is always raised |

Measured with a stub CLI that writes init, 15 assistant messages and an `is_error` result, then exits 1. The consumer reads one message, sleeps 300 ms, then drains. 100 runs, `Query` with `WithCanUseTool` so stdin stays open:

| | Old | New |
|---|---|---|
| Ended `ErrNoMoreMessages` though the CLI exited 1 | 29/100 | 0/100 |
| Lost the `ResultMessage` to an early `*ProcessError` | 17/100 | 0/100 |

(An earlier set of 100 runs on the same code measured 13/100 for the second row. It depends on scheduling.)

## Fix

New `streamReader` (`stream_reader.go`) reads one transport stream for both iterators:

- The transport sends the error after every message. So when the error is read from `errChan`, anything not yet in `msgChan` is never coming. The reader keeps the error as `pending`, hands out the messages still buffered, then returns the error.
- When `msgChan` closes, it returns an error still sitting in `errChan` before it returns `ErrNoMoreMessages`.
- It does not wait for `msgChan` to close after an error. A transport that sends an error and leaves `msgChan` open still returns the error as soon as the buffered messages are out.
- A closed `errChan` or `streamErrChan` is still "no more errors" for the call, as in #156.

`ClientImpl` owns one `streamReader` per connection and gives it to every `ReceiveResponse` iterator. This keeps #159 working: `ReceiveResponse` still ends at the `ResultMessage`, and an error the reader took early is not lost with that iterator. It comes out of the next `ReceiveResponse` or `Next` call.

`queryIterator` still closes the transport on every terminal path (#153), and `clientIterator` still marks itself closed on an error, a closed stream or a `ResultMessage`. `Next` in both is now a thin wrapper over `streamReader.next`.

## Behavior change

- No API change.
- `Next` now returns every message the CLI wrote before it returns the exit error. Before, it could return the error first.
- When `msgChan` closes with an exit error pending, `Next` returns that error once. Before, it could return `ErrNoMoreMessages`. After the error, the next call returns `ErrNoMoreMessages`.
- `ReceiveMessages()` returns the raw channel and is unchanged.

## Tests

The iterator tests feed hand-made channels. With a message and an error both ready, the old `select` picked either at random, so each scenario repeats 50 times on fresh channels. The old code failed on the first trial in every case below.

| Test | Result on old code |
|---|---|
| `TestIteratorsDeliverBufferedMessagesBeforeStreamError/{client,query}_iterator/error_ready_while_messages_buffered` | `trial 0: Next() #2 = Claude Code process exited unexpectedly (exit code: 1), want message m1 before the error` |
| `.../error_ready_with_msgchan_still_open` | `trial 0: Next() #1 = Claude Code process exited unexpectedly (exit code: 1), want message m0 before the error` |
| `.../msgchan_closed_with_error_in_closed_errchan` | `trial 0: Next() after 0 messages error = no more messages, want the exit error` |
| `TestQueryIteratorDeliversResultBeforeExitError` | `trial 0: ResultMessage lost to Claude Code returned an error result: boom (exit code: 1)` |
| `TestReceiveResponseExitErrorBelongsToNextCall` | `trial 0: turn Next() #1 = Claude Code returned an error result: boom (exit code: 1), want the turn's messages` |
| `TestTransportSendsExitErrorAfterLastMessage` (mock mode `burst_error_result_exit`, runs on Windows too) | passes on old code: it pins the transport order the iterators now rely on (the error arrives only after the last message is queued, `ResultMessage` last, `msgChan` closes after) |

Existing iterator tests (`TestIteratorsTreatClosedErrChanAsNoError`, `TestClientIteratorNextErrorPaths`, `TestClientIteratorStopsAfterResultMessage`) only change how they build the iterator.

## Gates

- [x] `gofmt -s -l .` clean
- [x] `go vet ./...` and `GOOS=windows go vet ./...` clean
- [x] `golangci-lint run ./...` (v2.4.0): 0 issues
- [x] `gocyclo -over 15 .`: no new entries in non-test code (`runMockCLI` stays at 19; the new mock mode went into `runShutdownMock`)
- [x] `make fuzz-test` passes
- [x] `ginkgo --no-color -r --race`: all 7 suites pass
- [x] The new root tests plus the existing iterator and `ReceiveResponse` tests pass `-count=200` under `-race`; the transport tests pass `-count=20`
- [x] CLAUDE.md and `internal/subprocess/CLAUDE.md` updated
- [x] Live stub probe, 100 runs: 0 ended `ErrNoMoreMessages`, 0 lost the `ResultMessage`

Related to #144 (follow-up to #156).
