Title: fix: Return the CLI initialize response from GetServerInfo

Branch: `feature/get-server-info-initialize-response`

## Summary

`Client.GetServerInfo` returns a hard-coded `{"connected": true, "transport_type": "subprocess"}`, even for a custom transport. Python's `get_server_info()` returns the initialize response the CLI sent during `connect()`. `Protocol.Initialize` already receives that response, but it decodes only `supported_commands` and drops the rest. `docs/parity.md` lists the row as PARITY.

| Part | Python | Go (this PR) |
|---|---|---|
| Keep the response | `Query.initialize()` stores `self._initialization_result = response` | `Protocol.Initialize` keeps the whole response map. `Protocol.InitializationResult()` returns a deep copy, nil before the handshake. `InitializeResponse.SupportedCommands` is decoded as before. |
| Return it | `get_server_info()` returns `_initialization_result` (None while `connect()` runs) | `GetServerInfo` returns it as `map[string]interface{}` (same signature). Each call returns a copy, so a caller cannot change what the next caller sees. |
| Not connected | `CLIConnectionError("Not connected. Call connect() first.")` | error `client not connected`, as today |

`subprocess.Transport.InitializationResult()` exposes the map while connected. It is not added to the public `Transport` interface, because that would break custom transports. `ClientImpl` type-asserts an unexported `serverInfoSource` interface, and the `Transport` doc comment describes the method. A custom transport without it gets `nil, nil` from `GetServerInfo`, the Go form of Python's `None`.

Live check (claude 2.1.286): the response has `account`, `agents`, `analytics_disabled`, `available_output_styles`, `commands` (57 entries), `current_permission_mode`, `fast_mode_state`, `models` (12 entries with `value`, `displayName`, `description`, `supportsEffort`, ...), `output_style`, `pid`, `session_state` and more. These are the keys the CLI sends, and Go now returns them unchanged.

## Python reference

- `src/claude_agent_sdk/client.py:540-564`: `get_server_info()` returns `getattr(self._query, "_initialization_result", None)`.
- `src/claude_agent_sdk/_internal/query.py:237`: `self._initialization_result: dict[str, Any] | None = None`.
- `src/claude_agent_sdk/_internal/query.py:358-363`: `initialize()` stores the response.

## Behavior change

- `GetServerInfo` no longer returns the `connected` and `transport_type` keys. Python has neither. `connected` is redundant: `GetServerInfo` already returns an error when the client is not connected, and the key said `true` even after the CLI had died. `transport_type` was always `"subprocess"`, even for a custom transport. Code that reads those keys gets nil. `examples/20_debugging_and_diagnostics` now prints a summary of the response.
- With a custom transport that does not implement `InitializationResult()`, `GetServerInfo` returns `nil, nil`.

Follow-up, not in this PR: Python has no typed accessor for the model roster. If you want one, a separate PR could add a `ModelInfo` struct (`value`, `resolvedModel`, `displayName`, `description`, `supportsEffort`, `supportedEffortLevels`) and a small helper that decodes `GetServerInfo()["models"]`. That helper needs no change to `Client` or `Transport`.

Noticed while testing: CLI 2.1.286 does not send `supported_commands` in its initialize response (see the live keys above), so `InitializeResponse.SupportedCommands` is always empty with the current CLI. This PR leaves that field alone.

## Tests

The "old code" column comes from a run against compile stubs (`InitializationResult()` returning nil, which matches main dropping the response) and, for `GetServerInfo`, from the unchanged main implementation.

| Test | Old code |
|---|---|
| `TestInitializationResultKeepsFullResponse` (commands, output styles, models, account; a caller's change to the result does not leak) | `InitializationResult() = map[], want map[account:map[subscriptionType:max] available_output_styles:[default Explanatory] commands:[...] models:[...] output_style:default]` |
| `TestTransportInitializationResult` (new mock `server_info`, real handshake over the pipe; nil before Connect and after Close) | `InitializationResult() = map[], want map[account:... models:[...] ...]` |
| `TestGetServerInfo/initialize_response`, `/after_query` | `GetServerInfo() = map[connected:true transport_type:subprocess], want map[available_output_styles:... models:[...] output_style:default]` |
| `TestGetServerInfo/custom_transport` | `GetServerInfo() = map[connected:true transport_type:subprocess], want map[]` |
| `TestGetServerInfo/not_connected`, `/after_disconnect` | pass (unchanged behavior) |
| `TestGetServerInfoConcurrent` | `GetServerInfo() = map[connected:true transport_type:subprocess], want the initialize response` |
| `TestSubprocessTransportKeepsServerInfo` | guard: fails if `subprocess.Transport` stops satisfying `serverInfoSource`, which would silently make `GetServerInfo` return nil |

`TestGetServerInfo` is now table-driven without per-name branches, and it leaves the gocyclo list.

## Test plan

- [x] All tests pass: `ginkgo -r --race` (all 7 packages, including the fuzz seed corpora that `make fuzz-test` runs): `Ginkgo ran 7 suites in 2m50s`, `Test Suite Passed`.
- [x] No linting errors: `golangci-lint run ./...` (v2.4.0): 0 issues.
- [x] Code follows style guidelines: `gofmt -s -l .` clean; `go vet ./...` and `GOOS=windows go vet ./...` clean; `gocyclo -over 15 .` shows no new entries (`TestGetServerInfo` left the list); `examples/20_debugging_and_diagnostics` builds and vets.
- [x] Documentation updated: `docs/reference.md`, `docs/architecture/interfaces.md`, `docs/parity.md` (the row now says what is returned), `CLAUDE.md`, `internal/control/CLAUDE.md`, `internal/subprocess/CLAUDE.md`. `docs/tracking/` has no row for `get_server_info`.
- [x] Commit messages follow conventions.

No related issue.
