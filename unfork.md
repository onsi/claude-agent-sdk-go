# Unfork plan: from `onsi/claude-agent-sdk-go` back to `severity1/claude-agent-sdk-go`

As of 2026-10-01. Upstream `main` = `c3bc12b` (v0.7.2 + #163). Fork `main` = `82c270f`. Pedagogue pins
the fork at `82c270f` through a `replace` in `go.mod`.

**Posted 2026-10-01.** PRs: A #164, B #165, C #166, D #167, E #168, F #169 (branches pushed to
`onsi/claude-agent-sdk-go` under their `feature/…` names). Comments posted on #149, #144, #147, #145,
#146 and #143, with the PR numbers filled in. Re-measured the dropped interrupt on CLI 2.1.286 (later turns 7/7 dropped, first turns 4/4 honoured) and filed it as anthropics/claude-code#98713.

## Picking this up

**State (2026-10-01):** all six PRs and the issue comments are posted; the next move is severity1's.
To pick this up:

1. **Read the threads:** `gh pr view <n> -R severity1/claude-agent-sdk-go --comments` for #164–#169,
   and `gh issue view <n> … --comments` for #149, #144 and #143.
2. **A closed PR does not mean rejected.** The maintainer usually ports a contributor's PR onto `main`
   himself and closes the original with a `Co-authored-by` credit (#140→#150, #142→#151). So check
   `main`/the latest tag for the *behaviour*, e.g. does `Client` have `Done`/`Err`? Does `GetServerInfo`
   return `models`? Don't go by PR state.
3. **Unfork when A, B and C are in a tagged release.** Then do "Pedagogue-side changes" below, steps 1–8.
   D and F only restore teardown latencies the fork already had; E is unused by pedagogue. None of
   them blocks.
4. **If the maintainer asks for changes:** keep Python-SDK parity as the argument (each PR body cites
   its Python file:line), follow CONTRIBUTING.md, and run CI's gates locally:
   - golangci-lint v2.4.0;
   - a Go 1.18 vet/build, since CI tests 1.18–1.23 on ubuntu/macos/windows.
5. **If B or C is rejected,** see "If a PR is rejected".
6. **anthropics/claude-code#98713** is the CLI's dropped interrupt. If it is fixed, step 3 of the
   pedagogue changes (the interrupt re-send) can be skipped. Re-run the repro in that issue against the
   new CLI to confirm.

## Summary

- Upstream fixed 4 of our 5 issues (#144, #145, #146, #147). #143 is still open.
- **#145 and #146 are fixed the way the fork fixed them**, re-verified against `c3bc12b`.
- **#147 is fixed the same way.** The fork also re-sends an interrupt the CLI drops while it is idle. Upstream won't take that, because it is not Python behaviour. It moves into pedagogue (see "What goes where").
- **#144 is only half fixed.** Their fix covers the `ReceiveResponse`/`Query` iterators. A host on `ReceiveMessages`, which is what pedagogue uses, still can't see the CLI die. Their fix also has a race, measured over 100 runs:
  - 13% of Query runs lose the turn's `ResultMessage`.
  - 29% lose the exit error.
  - The fork is 0/100 on both.
- **Pedagogue builds against upstream except for three symbols:** `Client.Done()`, `Client.Err()` and `SupportedModels`/`ModelInfo`.
- **Six PRs are drafted, each green on its own:**
  - Two close the pedagogue gap (B: `Done`/`Err`; C: `GetServerInfo` returns the initialize response, which carries the model list).
  - One fixes their #144 race (A).
  - Two port lifecycle fixes the fork already had (D, F).
  - One closes #143 (E).
- **With A+B+C applied, pedagogue's harness and server suites pass**, apart from two pedagogue-side changes and a test artifact of the prototype (see [Validation](#validation)).
- **What unforking needs:** A, B and C merged and tagged, then about 150 lines of pedagogue changes. D and F restore behaviour we have today. E is optional.

## What pedagogue uses from the fork

Compiling pedagogue against upstream `c3bc12b` (via a scratch `-modfile`) fails on exactly:

| Symbol | Used by | Upstream answer |
|---|---|---|
| `Client.Done()` | `Harness.receive`, `Librarian.receive`: notice the child's exit even while a descendant holds its stdout | none; PR B |
| `Client.Err()` | the same receive loops: why the child ended | none; PR B |
| `Client.SupportedModels()` / `ModelInfo` | `harness.ProbeModels` → `server.modelRoster` | none; PR C makes `GetServerInfo()` return the initialize response, and pedagogue decodes `models` itself |

Everything else pedagogue imports (about 100 identifiers) exists upstream with the same shape.

## Our issues: upstream's fix against the fork's

| Issue | Upstream fix | Fork fix | Verdict |
|---|---|---|---|
| **#147** Interrupt sends SIGINT | #154: `Transport.Interrupt` → `protocol.Interrupt`; no signal fallback, since every connection now has the control protocol | `d57f6be`, the same | **In line.** The fork's extra, re-sending an interrupt the idle CLI dropped (`374037a`, `6204a1c`), is a workaround for CLI behaviour. It moves to pedagogue. |
| **#146** CanUseTool ignored by `Query` | #134 put `Query` on streaming always; #152 runs `prepareOptions` for `Query`, and `CanUseTool` with a non-stdio `PermissionPromptToolName` is an error | `c2d11ba`: streams only when callbacks need it, otherwise keeps `--print` | **In line, and upstream's is better** (one transport path, Python's mutual-exclusion check). Re-verified: `Query` argv now carries `--permission-prompt-tool stdio`. |
| **#145** drained `Query` leaks a zombie and temp files | #150 (single Wait owner) plus #153 (`Next` closes on every terminal path) | `5de76d2` plus `a19200c` | **In line.** Re-verified: 3 queries drained without `Close` leave 0 zombies and 0 temp files. |
| **#144** dead CLI unobservable | #156: the exit becomes a `*ProcessError` on the error channel; `SendMessage` after exit returns a `*ConnectionError`; `IsConnected` is false at transport level | `a80dbbd`: `Client.Done()`/`Err()`; every call on a dead client returns the exit error, `QueryStream` included | **Only half fixed** — see below. |
| **#143** per-task controls | open (tracker rows 10/11 pending) | `b22e3a2`: typed task messages, `StopTask`, `BackgroundTasks` | PR E, Python-shaped |

### What #144 still lacks upstream (measured on `c3bc12b`, stub CLI, no API calls)

- After `SIGKILL` of the child:
  - The `ReceiveMessages` channel closes with no reason.
  - `QueryStream` returns `nil`; the write error sits in `streamErrChan`, which only `ReceiveResponse` reads.
  - `GetServerInfo` still says `connected:true`.
  - Only the synchronous `Query` returns a `*ConnectionError`.
- A CLI child that exits while a descendant holds its stdout:
  - The child is reaped at once, but `ReceiveMessages` stays open until the descendant exits (8 s in the probe).
  - Nothing tells the host the CLI is gone.
  - Pedagogue's `receive` loops exist for exactly this case.
- **The #156 race.** `handleStdout` queues messages in a 10-slot `msgChan`, then sends the exit error to `errChan`, then closes both, and the iterators `select` across the two channels. Over 100 runs of a stub that sends 15 messages and an `is_error` result and exits 1, with a slow consumer:
  - The `ResultMessage` and the messages around it were lost in 13–17 runs.
  - The run ended in `ErrNoMoreMessages` in 29: the exit error was lost.
  - The fork was 0/100 on both: it derives the exit error only after `msgChan` closes.
  - Pedagogue's `OneShotReport.drain` relies on "result, then `*ProcessError`".

## Other upstream regressions against the fork (measured)

| | Upstream `c3bc12b` | Fork | Fix |
|---|---|---|---|
| `Interrupt` racing `Disconnect` | waits out the 5 s control timeout | 200 ms (`ErrProtocolClosed`, `3058ffd`) | PR D |
| CLI exits while a control request is pending | the request waits out its 5 s timeout | same as upstream | PR D. Python fails pending requests when the reader errors (`query.py:539-546`). |
| `Disconnect` when a descendant holds the CLI's stdout | 5 s | 0 s (`5de76d2` closes the read ends first) | PR F |

### Found in passing (verified, not drafted)

- **A second `Connect` without a `Disconnect` leaks the first CLI.**
  - Its PID survives `Disconnect`, because there is no already-connected check.
  - Pedagogue never does this.
  - Worth an issue later; not drafted.
- **A non-file `WithDebugWriter` delays exit detection.**
  - `os/exec` copies stderr on a goroutine, and `cmd.Wait` waits for that copy, so a descendant holding stderr delays `processDone`.
  - Pedagogue does not use `WithDebugWriter`.
  - Noted as a known limitation in PR B.
- **CLI 2.1.286 sends no `supported_commands`** in its initialize response, so upstream's `InitializeResponse.SupportedCommands` is always empty. It sends `commands` (57) and `models` (12). Noted in PR C.

## What goes where: every fork feature

| Fork commit | Feature | Destination |
|---|---|---|
| `d57f6be` | Interrupt over the control protocol | **upstream already** (#154) |
| `5de76d2` | single Wait owner, `os.Pipe` stdout | **upstream already** (#150) |
| `5de76d2` | `Close` closes the read ends before waiting for readers | **PR F** |
| `a80dbbd` | `Client.Done()`/`Err()`, fail-fast on a dead client, `QueryStream` reports it | **PR B** |
| `a80dbbd`/`a19200c` | exit error delivered after every buffered message | **PR A** |
| `a19200c` | drained `Query` releases the CLI | **upstream already** (#153) |
| `b22e3a2` | typed `task_*` messages, `StopTask` | **PR E** (Python's field set; extra CLI fields stay in `Data`) |
| `b22e3a2` | `BackgroundTasks` | **drop.** TS-only, pedagogue doesn't use it; offered as a follow-up in PR E's text |
| `3058ffd` | pending control requests fail on `Close` | **PR D** |
| `dab5769` | one stdin writer, race-free handshake state, control calls release `t.mu` | **upstream already** (#150, #163) |
| `c2d11ba` | `Query` honours callbacks | **upstream already** (#134, #152) |
| `374037a`, `6204a1c` | re-send an interrupt the idle CLI dropped | **pedagogue** (see "Pedagogue-side changes", step 3); optionally report the CLI behaviour to `anthropics/claude-code` |
| `82c270f` | `SupportedModels()` from the initialize response | **PR C** (`GetServerInfo` parity) plus about 25 lines in pedagogue |
| — | `os/exec` closes stdin when the child is reaped | **upstream already** (it uses `cmd.StdinPipe` too) |

## The drafted PRs

Each is one or two commits off `c3bc12b`, written to CONTRIBUTING.md:
- conventional commits with `(Issue #N)`, and `feature/issue-N-…` branches;
- TDD (each body records the old-code failure text);
- table-driven tests using the TestMain mock CLI, with no `/bin/sh` and no build tags;
- docs and the parity tracker updated;
- the PR-requirements checklist in each body.

Each body uses the maintainer's own PR layout: Summary, Python reference with file:line, Behavior change, a Tests table, and the gates. The gates are:
- the full `-race` suite (via the ginkgo CLI);
- `gofmt -s`;
- `go vet` for both darwin and `GOOS=windows`;
- golangci-lint at CI's pinned v2.4.0;
- `gocyclo -over 15` with no new entries;
- `make fuzz-test`.

| PR | Branch | Title | Pedagogue needs it? | Parity basis |
|---|---|---|---|---|
| **A** | `feature/issue-144-exit-error-after-buffered-messages` | fix: deliver buffered messages before the CLI exit error (Issue #144) | **yes** (correctness of one-shots) | Python raises `ProcessError` only after every message is read |
| **B** | `feature/issue-144-client-done-err` | feat: Add Client.Done and Client.Err to report CLI process exit (Issue #144) | **yes** (compile) | Python's `receive_messages()` *raises* the `ProcessError`; a Go channel can't, so this is the `context.Context` analogue |
| **C** | `feature/get-server-info-initialize-response` | fix: Return the CLI initialize response from GetServerInfo | **yes** (model list) | Python `get_server_info()` returns `_initialization_result`; upstream returns a hard-coded map |
| **D** | `feature/fail-pending-control-requests-on-close` | fix: fail pending control requests when the CLI exits or the protocol closes | nice to have (we have it today) | CLI-exit half: `query.py:539-546`. Close half: a Go lifecycle fix, stated as such. |
| **F** | `feature/close-does-not-wait-for-stdout-holders` | fix: close does not wait for a descendant that holds the CLI's stdout | nice to have (we have it today) | Python `close()` never waits for stdout EOF (`subprocess_cli.py:997-1058`) |
| **E** | `feature/issue-143-task-lifecycle` | feat: Add typed task lifecycle messages and StopTask (Issue #143) | no | tracker rows 10 (partial) and 11; Python `types.py`/`message_parser.py`/`stop_task` |

### What each PR changes, and the decisions to check

- **A** adds a `streamReader` shared by both iterators.
  - An error read early is held back until the messages already buffered are delivered.
  - An error left in a closed `errChan` is not lost.
  - `ClientImpl` owns one per connection, so `ReceiveResponse` still stops at the `ResultMessage` (#159), and an early error surfaces on the next call.
  - Stub result: 0/100 lost either way (was 13–17/100 and 29/100).
- **B** adds `Done() <-chan struct{}` and `Err() error` on `Client`, read from the subprocess transport through an unexported optional interface, so the `Transport` interface is untouched.
  - `Err()` is:
    - nil while the CLI runs;
    - a `*ProcessError` on a non-zero exit (-1 for a signal);
    - a `*ConnectionError` on a clean exit;
    - "client not connected" before `Connect` and after `Disconnect`.
  - Calls on a dead client return `NewConnectionError("cannot write to terminated CLI process", Err())`, which matches Python's `CLIConnectionError`. `errors.As` finds both error types.
  - Behaviour change: anyone implementing `Client` must add two methods.
  - *Decision for you:* the clean-exit `*ConnectionError` makes `Err()` non-nil whenever `Done` is closed, as with `context.Context`. The fork's semantics differed slightly. Pedagogue's suites pass either way.
- **C** keeps the full initialize response; `GetServerInfo` returns a deep copy (nil, nil for a custom transport).
  - Behaviour change: the `connected`/`transport_type` keys go away (Python has none). `examples/20` is updated.
  - A typed `SupportedModels()` is deliberately left out (Python has none); it could be a follow-up.
- **D**, two commits:
  - `ErrProtocolClosed`: `Close` wakes every waiting request, and a request after `Close` fails without being written.
  - `Protocol.FailPendingRequests(err)`: called from `handleStdout` on the exit `*ProcessError` and on a scanner error, before the error reaches `errChan`. An `initialize` in flight is left to the existing init-error routing, to avoid failing it twice.
  - Results: `Interrupt` racing `Disconnect` 5 s → 200 ms. `Interrupt` when the CLI dies 5 s → 0 s.
- **F**: once the CLI is reaped, close the stdout read end before waiting for readers.
  - The stderr callback gets a bounded 500 ms to drain, so a CLI that dies during `Connect` doesn't lose its last stderr lines.
  - #163's EOF grace is untouched.
  - Result: 5 s → 0 s.
- **E**: `TaskStartedMessage`/`TaskProgressMessage`/`TaskNotificationMessage`/`TaskUpdatedMessage` with Python's field set and JSON names, plus `TaskUsage`, the status enums, `IsTerminalTaskStatus` and `Client.StopTask`.
  - *Decision for you:* task messages still arrive as `*SystemMessage`, with typed views via `msg.AsTaskStarted()` etc. Delivering the typed structs directly would silently drop them from every existing `case *SystemMessage` arm. Python avoids this with subclasses, which Go lacks.
  - *Decision for you:* a `task_started`/`task_progress`/`task_notification` missing a required key now gives a `MessageParseError`, as Python's parser does.
    - On a `Query` iterator, that error ends the iteration.
    - Python behaves the same, but it is a new way for a malformed CLI line to end a turn.
    - The alternative is to fall back to a plain `SystemMessage`; pick one.
  - `StopTask` is added to the public `Transport` interface (the `GetMcpStatus` precedent), which breaks custom transports.
  - The tracker rows use `#TBD` for the PR number.

### Merge order and conflicts

The PRs conflict mechanically with each other in `client.go` (A and B both edit `ClientImpl`), `internal/subprocess/mock_cli_test.go` (new mock modes) and the CLAUDE.md files. They have no semantic conflicts: I merged A+B+C into one branch and resolved it in a few minutes.

The maintainer's habit is to port PRs onto `main` himself (#140→#150, #142→#151), so overlapping PRs are cheap for him. Suggested order, with each PR opened only after the previous one lands and rebased onto it:

1. **A**: a bug in their own fresh fix, small, no API.
2. **D**, then **F**: small lifecycle fixes.
3. **C**: a parity fix with a visible behaviour change.
4. **B**: new public API, and the one most likely to need discussion. Opening it as a comment on #144 first (draft below) lets the maintainer weigh in on the shape before the PR.
5. **E**: last, and optional for us.

Opening all six at once is also defensible: he triages in batches, and the bodies don't depend on each other. Your call.

## Comments (posted 2026-10-01; text below is the draft, the posted versions name the PRs)

### #149 "Is this being maintained?" (reply)

> Thank you for the update, and for working through the backlog so quickly.
>
> I re-tested our reports against main (`c3bc12b`). #145, #146 and #147 behave as fixed with our
> original reproducers. A few things remain, and I have PRs ready for each, written to
> CONTRIBUTING.md and checked against the Python SDK:
>
> - #156 (the #144 fix) has an ordering race: the exit error can overtake the turn's buffered
>   messages, or be lost. Details on #144.
> - A host that reads `ReceiveMessages()` still cannot see the CLI exit. Details on #144.
> - `GetServerInfo` returns a fixed map. Python returns the initialize response.
> - A pending control request waits out its timeout when the CLI exits or the client closes.
>   Python fails it at once when the CLI exits.
> - `Close` waits 5 s when a process the CLI started still holds its stdout.
> - #143: typed task messages and `StopTask`, shaped on Python's types.
>
> I'm gonna open them all up at once.  But if you'd rather have them sequenced let me know.

### #144 (closed): what remains

> Thanks for #156. I re-tested on `c3bc12b` with a stub CLI, and two things remain.
>
> **1. The exit error can overtake buffered messages, or be lost.** `handleStdout` queues messages
> in `msgChan` (10 slots), sends the `*ProcessError` to `errChan`, then closes both. The iterators
> `select` across the two channels, so when both are ready the pick is random. A stub writes 15
> messages and an `is_error` result, then exits 1. The consumer reads one message, sleeps 300 ms,
> then drains. Over 100 runs of `Query`:
>
> | | runs |
> |---|---|
> | `ResultMessage` and trailing messages lost to an early `*ProcessError` | 13–17 / 100 |
> | iteration ended `ErrNoMoreMessages` though the CLI exited 1 | 29 / 100 |
>
> Python raises the `ProcessError` only after every message has been read. I have a PR that keeps
> that order for both iterators, with deterministic tests.
>
> **2. A host on `ReceiveMessages()` still has no signal.** This is the path the README's streaming
> example uses. After the CLI is killed:
>
> - the channel closes with no reason;
> - `QueryStream` returns `nil`, and its write error goes to `streamErrChan`, which only
>   `ReceiveResponse` reads;
> - when a process the CLI started holds its stdout, the channel stays open after the CLI has
>   exited and been reaped (8 s in the probe).
>
> Python's `receive_messages()` raises the `ProcessError`. A Go channel cannot carry an error, so I'd
> propose the `context.Context` pattern:
>
> - `Client.Done() <-chan struct{}` closes when the CLI process exits;
> - `Client.Err() error` says why;
> - calls on a dead client fail fast with a `*ConnectionError` that wraps it, like Python's
>   `CLIConnectionError`.
>
> `Transport` stays unchanged; the subprocess transport provides the methods through an optional
> interface. I have this as a PR too. Would you take that shape, or do you prefer another?

### #147 (closed): one CLI behaviour we work around

> Confirmed: `Interrupt` now keeps the session, and the next turn uses the same process. Thanks.
>
> One CLI behaviour, for anyone who lands here. An interrupt that arrives after a user message is
> written, but before the CLI has picked that message up, finds the CLI idle. The CLI drops it, then
> runs the queued turn in full. We measured this on CLI 2.1.274. We re-send the interrupt from the
> host when the turn's `system`/`init` message arrives. Python and TypeScript do not do this, so I'm
> not proposing it for the SDK.

*(Re-measure this on the current CLI before posting. It costs one small live turn.)*

### #145 and #146 (closed)

Optional; they need no reply. If you want one:

> Re-tested on `c3bc12b` with the original reproducer: fixed. Thank you.

### #143 (open): accompanies PR E

> PR #NNN implements tracker row 11 and the `stop_task` part of row 10, shaped on Python's types:
>
> - `TaskStartedMessage`, `TaskProgressMessage`, `TaskNotificationMessage` and
>   `TaskUpdatedMessage`, with Python's fields;
> - `IsTerminalTaskStatus`;
> - `Client.StopTask`.
>
> The task messages still arrive as `*SystemMessage`, and `AsTaskStarted()` and the other `As…`
> methods give the typed view. That way existing `case *SystemMessage` code keeps seeing them.
>
> `Interrupt` already uses the control protocol (#154). The `background_tasks` request exists only in
> the TypeScript SDK, so I left it out. I can send it separately if you want it.

### Optional: `anthropics/claude-code` issue

> **An interrupt control request that arrives before the CLI picks up a queued user message is
> dropped, and the turn then runs in full.** Stream-json input mode. The host writes a `user`
> message, then sends `{"subtype":"interrupt"}` before the CLI emits that turn's `system`/`init`.
> The CLI acknowledges the interrupt, finds no turn running, and ignores it. It then starts the
> queued turn and runs it to completion. Expected: the queued turn is cancelled, or the interrupt
> applies to it. Measured on 2.1.274.

*(Re-verify on the current CLI first.)*

## Pedagogue-side changes, once A, B and C are in an upstream release

A prototype of all of this is in `unfork-drafts/pedagogue-prototype.diff`. It was built and run in a
scratch copy; the pedagogue repo is untouched.

1. **`go.mod`**: require the release with A, B and C; delete the `replace`; `go mod tidy`.
2. **Model roster**:
   - `harness.ProbeModels` decodes `GetServerInfo()["models"]` into a pedagogue-owned `ModelInfo` (same six fields).
   - `internal/server` switches from `claudecode.ModelInfo` to it.
   - About 25 lines; the roster's undocumented-shape fallback is unchanged.
3. **Interrupt re-send**, one code path for the harness and the librarian:
   - Open the window when a turn is dispatched (`Query`/`QueryStream`), close it on the turn's `system`/`init` or on a `ResultMessage`.
   - A stop inside the window is re-sent once, at `init`.
   - The prototype is a client decorator applied where `newClient()` is called, and it passes `stub_cli_test.go`'s pickup spec unchanged.
   - Decide between the decorator and inline state. The decorator must forward the optional `mcpStatusClient` interface, and it breaks three specs that downcast the client to `*mcpStatusFakeClient`. Inline state avoids both.
4. **`stub_cli_test.go`**: `orphanedStdoutStub` must answer the `initialize` request before exiting. Upstream always handshakes, as Python does; the fork only did when callbacks needed it.
5. **Comments and docs:**
   - `fence.go`'s comment that `WithCanUseTool` "moves claudecode.Query onto streaming input" is stale: `Query` always streams now.
   - Rewrite `design/sdk-upstream-issues.md`. It says *never post to severity1*, and its fork tables become history.
   - Update `design/architecture.md` (line 55) and `design/agent-seam.md`, which reference the fork.
6. **Upstream behaviour changes to absorb (audited, none blocking):**
   - **#163: the `Connect` ctx no longer ends the session.**
     - Harness and librarian always `Disconnect` in `Stop`, so nothing relied on cancellation.
     - Re-check the `die` path: it marks a harness unusable without disconnecting, and `Stop` reaps later.
   - **#163: `Close` can take up to about 15 s** if the CLI ignores both stdin EOF and SIGTERM (5 s EOF grace, then SIGTERM + 5 s, then SIGKILL + 5 s). A normal close measured about 600 ms upstream. Check the macapp's quit path tolerates this.
   - **`Query` always does the initialize handshake.** Pedagogue's one-shots already streamed (they pass `WithCanUseTool`), so they see no latency change.
   - Smaller changes:
     - `updatedInput` is always sent on allow;
     - an unknown control subtype gets an error reply;
     - `WithExtraArgs` values that start with `-` are now sent as `--flag=value` (ours is `--strict-mcp-config` with a nil value, so it is unaffected);
     - on Windows, a `.cmd`/`.bat` CLI is refused.
7. **Gates**, per CLAUDE.md:
   - `make test-go`, `make test-e2e`, `make test-macapp` (it pins the `serve` boot line);
   - `make smoke` (live) last, which exercises interrupt and model switching against the real CLI;
   - a live check that `ProbeModels` still returns the roster.
8. **Retire the fork:** archive `onsi/claude-agent-sdk-go` and add a README pointer. Keep it readable, since pedagogue's history pins its pseudo-versions.

### If a PR is rejected

| PR | Fallback |
|---|---|
| A rejected | Pedagogue's `drain` can treat "`*ProcessError` with no result seen" as possibly a lost result. That only papers over it. Push for a fix in whatever shape the maintainer prefers. |
| B rejected | No clean fallback. `ReceiveResponse` per turn gets the exit error, but nothing reports a child that exits while a descendant holds stdout. Ask the maintainer for the shape he'd accept (the #144 comment does this), rather than keeping a fork. |
| C rejected | `ProbeModels` failing costs only the roster enrichment, by design. Pedagogue degrades to the curated list. Acceptable, but ask first. |
| D or F rejected | Pedagogue lives with a 5 s teardown wait in rare cases. Acceptable. |
| E rejected | Nothing; pedagogue doesn't use it. |

## Validation

On `c3bc12b` with a stub CLI (`scratchpad/probe`, `probe2`), no API calls:
- #145 and #146 are fixed;
- the #144 gaps, the #156 race, and the D and F regressions all reproduce, with the numbers above.

Pedagogue's `internal/harness` and `internal/server` suites, in a scratch copy of pedagogue `HEAD`:

| Built against | Result |
|---|---|
| the fork | 1144 + 1299 specs pass (baseline) |
| upstream + A+B+C, with steps 2 and 4 | server 1299/1299 pass; harness fails only the pickup spec, as expected |
| the same, plus the step 3 prototype | the pickup spec passes; the 3 failures left are specs that downcast the client to a test fake (the decorator caveat in step 3) |

D, E and F were not in that integration run. They don't touch any API pedagogue calls.

Every PR branch, re-run by me (not the drafting agents), one at a time on an otherwise idle machine:

| Branch (local name) | Commit | `ginkgo -r --race` | golangci-lint v2.4.0 | gofmt |
|---|---|---|---|---|
| `unfork/pr-A-exit-error-after-buffered-messages` | `1e96ba8` | 7 suites, passed (2m50s) | 0 issues | clean |
| `unfork/pr-B-client-done-err` | `a1ec660` | 7 suites, passed (3m02s) | 0 issues | clean |
| `unfork/pr-C-get-server-info` | `7a5cf28` | 7 suites, passed (2m50s) | 0 issues | clean |
| `unfork/pr-D-fail-pending-control-requests` | `e3d8d03` | 7 suites, passed (2m53s) | 0 issues | clean |
| `unfork/pr-E-task-lifecycle` | `12187b5` | 7 suites, passed (2m51s) | 0 issues | clean |
| `unfork/pr-F-close-does-not-wait` | `7b112db` | 7 suites, passed (2m52s) | 0 issues | clean |

The drafting agents also stress-ran each PR's new tests (`-count` 10–300) and ran `make fuzz-test`.
I have not re-run those myself.

## Where things are

- **PRs:** severity1/claude-agent-sdk-go #164 (A), #165 (B), #166 (C), #167 (D), #168 (E), #169 (F).
  Their branches are on `onsi/claude-agent-sdk-go` under the `feature/…` names in the PR table.
  The local `unfork/pr-*` branches in this repo point at the same commits; E's was amended to fill in #168.
- **PR bodies as drafted:** `unfork-drafts/pr-{A,B,C,D,E,F}.md`. The posted versions are on GitHub.
- **Pedagogue prototype** (steps 2–4): `unfork-drafts/pedagogue-prototype.diff`, against pedagogue `81e3ac0e`.
- **Stub-CLI probes** (the measurements above): rebuildable from this file's descriptions. The
  scratch copies were session-local and are gone.
