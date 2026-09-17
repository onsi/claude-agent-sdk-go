//go:build !windows

package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// queryControlStubCLI answers initialize, then answers the prompt with a
// control request — mcp_message when given an MCP config, hook_callback when
// initialize registered hooks, can_use_tool otherwise — and finishes the turn
// once the SDK replies. It exits on stdin EOF.
const queryControlStubCLI = `
dir=%q
printf '%%s\n' "$*" > "$dir/argv"
hooks=
mcp=
case "$*" in *--mcp-config*) mcp=1 ;; esac
while IFS= read -r line; do
  printf '%%s\n' "$line" >> "$dir/stdin.log"
  case "$line" in
    *'"type":"control_response"'*)
      printf '%%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"model":"stub"}}'
      printf '%%s\n' '{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"num_turns":1,"session_id":"s1"}' ;;
    *'"subtype":"initialize"'*)
      case "$line" in *hookCallbackIds*) hooks=1 ;; esac
      req_id=$(printf '%%s' "$line" | grep -o '"request_id":"[^"]*"' | cut -d'"' -f4)
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$req_id" ;;
    *'"type":"user"'*)
      if [ -n "$mcp" ]; then
        printf '%%s\n' '{"type":"control_request","request_id":"cli_1","request":{"subtype":"mcp_message","server_name":"calc","message":{"jsonrpc":"2.0","id":7,"method":"tools/list"}}}'
      elif [ -n "$hooks" ]; then
        printf '%%s\n' '{"type":"control_request","request_id":"cli_1","request":{"subtype":"hook_callback","callback_id":"hook_0","input":{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}}}'
      else
        printf '%%s\n' '{"type":"control_request","request_id":"cli_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"}}}'
      fi ;;
  esac
done
`

func runControlQuery(t *testing.T, opts ...Option) (argv, stdin string) {
	t.Helper()
	dir := t.TempDir()
	cliPath, pidFile := writeLifecycleCLI(t, fmt.Sprintf(queryControlStubCLI, dir))

	iter, err := Query(context.Background(), "list the files", append(opts, WithCLIPath(cliPath))...)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = iter.Close() }()
	count, err := drainQuery(t, iter)
	if !errors.Is(err, ErrNoMoreMessages) {
		t.Errorf("drain ended with %v after %d messages", err, count)
	}
	if count != 2 {
		t.Errorf("received %d messages, want the assistant reply and the result", count)
	}
	assertReaped(t, readLifecyclePID(t, pidFile))

	argvData, err := os.ReadFile(filepath.Join(dir, "argv")) // #nosec G304 -- path is under t.TempDir
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	stdinData, err := os.ReadFile(filepath.Join(dir, "stdin.log")) // #nosec G304 -- path is under t.TempDir
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read stdin log: %v", err)
	}
	return string(argvData), string(stdinData)
}

func TestQueryHonoursCanUseTool(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	gate := func(_ context.Context, name string, input map[string]any, _ ToolPermissionContext) (PermissionResult, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, fmt.Sprintf("%s %v", name, input["command"]))
		return NewPermissionResultDeny("not on my watch"), nil
	}

	argv, stdin := runControlQuery(t, WithCanUseTool(gate))

	for _, want := range []string{"--permission-prompt-tool stdio", "--input-format stream-json"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q lacks %q", argv, want)
		}
	}
	if strings.Contains(argv, "--print") || strings.Contains(argv, "list the files") {
		t.Errorf("argv %q still carries the prompt on the command line", argv)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != "Bash rm -rf /" {
		t.Errorf("permission callback calls = %q, want one for Bash", asked)
	}
	mu.Unlock()
	if !strings.Contains(stdin, `"content":"list the files"`) {
		t.Errorf("prompt was not written to stdin:\n%s", stdin)
	}
	if !strings.Contains(stdin, `"behavior":"deny","message":"not on my watch"`) {
		t.Errorf("deny decision was not sent to the CLI:\n%s", stdin)
	}
}

func TestQueryHonoursHooks(t *testing.T) {
	const gatedTool = "Bash"
	var mu sync.Mutex
	var seen []string
	hook := func(_ context.Context, input any, _ *string, _ HookContext) (HookJSONOutput, error) {
		mu.Lock()
		defer mu.Unlock()
		if pre, ok := input.(*PreToolUseHookInput); ok {
			seen = append(seen, pre.ToolName)
		}
		decision, reason := "block", "blocked by hook"
		return HookJSONOutput{Decision: &decision, Reason: &reason}, nil
	}

	_, stdin := runControlQuery(t, WithPreToolUseHook(gatedTool, hook))

	mu.Lock()
	if len(seen) != 1 || seen[0] != gatedTool {
		t.Errorf("hook calls = %q, want one for Bash", seen)
	}
	mu.Unlock()
	if !strings.Contains(stdin, `"hookCallbackIds":["hook_0"]`) {
		t.Errorf("initialize did not register the hook:\n%s", stdin)
	}
	if !strings.Contains(stdin, `"request_id":"cli_1","response":{"decision":"block","reason":"blocked by hook"}`) {
		t.Errorf("hook decision was not sent to the CLI:\n%s", stdin)
	}
}

func TestQueryWithoutControlCallbacksKeepsPromptArgument(t *testing.T) {
	dir := t.TempDir()
	cliPath, _ := writeLifecycleCLI(t, fmt.Sprintf("printf '%%s\\n' \"$*\" > %q\n%s", filepath.Join(dir, "argv"), transcriptBody(0)))
	iter, err := Query(context.Background(), "hello", WithCLIPath(cliPath))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if _, err := drainQuery(t, iter); !errors.Is(err, ErrNoMoreMessages) {
		t.Errorf("drain ended with %v", err)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "argv")) // #nosec G304 -- path is under t.TempDir
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(argv)), "--print hello") {
		t.Errorf("argv = %q, want the prompt passed with --print", argv)
	}
}

func TestQueryHonoursSdkMcpServers(t *testing.T) {
	add := NewTool("add", "adds numbers", map[string]any{"type": "object"},
		func(context.Context, map[string]any) (*McpToolResult, error) { return &McpToolResult{}, nil })

	_, stdin := runControlQuery(t, WithSdkMcpServer("calc", CreateSDKMcpServer("calc", "1.0.0", add)))

	if !strings.Contains(stdin, `"request_id":"cli_1"`) || !strings.Contains(stdin, `"name":"add"`) {
		t.Errorf("tools/list was not answered by the SDK server:\n%s", stdin)
	}
}
