//go:build !windows

package claudecode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// pickupStubCLI models a CLI that drops an interrupt arriving while it is
// idle. A prompt containing "gapN" makes it wait for N such interrupts before
// it announces the turn with its init message, a prompt containing "noinit"
// ends the turn on the next interrupt without ever announcing it, and any
// other prompt starts the turn at once. Once the turn is running, an
// interrupt ends it with an error result; without one the turn answers in
// full.
const pickupStubCLI = `#!/bin/bash
if [ "$1" = "-v" ]; then echo "3.0.0"; exit 0; fi
log=%q
init='{"type":"system","subtype":"init","session_id":"s1"}'
assistant='{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"full turn"}],"model":"stub"}}'
stopped='{"type":"result","subtype":"error_during_execution","duration_ms":1,"duration_api_ms":1,"is_error":true,"num_turns":1,"session_id":"s1"}'
finished='{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"num_turns":1,"session_id":"s1"}'

ack() {
  req_id=$(printf '%%s' "$1" | grep -o '"request_id":"[^"]*"' | cut -d'"' -f4)
  printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$req_id"
}

run_turn() {
  printf '%%s\n' "$init"
  if IFS= read -r -t 2 line; then
    printf '%%s\n' "$line" >> "$log"
    case "$line" in
      *'"subtype":"interrupt"'*)
        ack "$line"
        printf '%%s\n' "$stopped"
        return ;;
      *'"type":"control_request"'*) ack "$line" ;;
    esac
  fi
  printf '%%s\n' "$assistant"
  printf '%%s\n' "$finished"
}

want=0
idle=0
noinit=0
while IFS= read -r line; do
  printf '%%s\n' "$line" >> "$log"
  case "$line" in
    *'"subtype":"interrupt"'*)
      ack "$line"
      idle=$((idle+1))
      if [ "$noinit" -eq 1 ]; then
        noinit=0
        printf '%%s\n' "$finished"
      elif [ "$want" -gt 0 ] && [ "$idle" -ge "$want" ]; then
        want=0
        run_turn
      fi ;;
    *'"type":"control_request"'*)
      ack "$line" ;;
    *'"type":"user"'*)
      idle=0
      case "$line" in
        *gap2*) want=2 ;;
        *gap*) want=1 ;;
        *noinit*) noinit=1 ;;
        *) run_turn ;;
      esac ;;
  esac
done
`

func connectPickupStubClient(ctx context.Context, t *testing.T) (Client, <-chan Message, string) {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		t.Skip("stub CLI is a bash script")
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "stdin.log")
	cliPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(cliPath, []byte(fmt.Sprintf(pickupStubCLI, logPath)), 0o755); err != nil { //nolint:gosec // G306: test stub must be executable
		t.Fatalf("write stub CLI: %v", err)
	}

	client := NewClient(WithCLIPath(cliPath))
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	return client, client.ReceiveMessages(ctx), logPath
}

func countInterrupts(log string) int {
	return strings.Count(log, `"subtype":"interrupt"`)
}

// awaitStoppedTurn fails if the turn answers in full instead of ending on an
// error result.
func awaitStoppedTurn(t *testing.T, msgs <-chan Message) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("message channel closed before the turn ended")
			}
			switch m := msg.(type) {
			case *AssistantMessage:
				t.Errorf("the interrupted turn answered: %#v", m.Content)
			case *ResultMessage:
				if !m.IsError {
					t.Errorf("turn ended with %s, want the interrupted result", m.Subtype)
				}
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for the turn to end")
		}
	}
}

func TestInterruptInPickupGapIsRepeatedOnTurnStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, msgs, logPath := connectPickupStubClient(ctx, t)

	if err := client.Query(ctx, "gap turn"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if err := client.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	awaitStoppedTurn(t, msgs)

	if got := countInterrupts(stubStdinLog(t, logPath)); got != 2 {
		t.Errorf("CLI received %d interrupts, want the dropped one and its repeat", got)
	}
}

func TestTwoInterruptsInPickupGapProduceOneRepeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, msgs, logPath := connectPickupStubClient(ctx, t)

	if err := client.Query(ctx, "gap2 turn"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := client.Interrupt(ctx); err != nil {
			t.Fatalf("Interrupt %d: %v", i, err)
		}
	}

	awaitStoppedTurn(t, msgs)

	if got := countInterrupts(stubStdinLog(t, logPath)); got != 3 {
		t.Errorf("CLI received %d interrupts, want the two dropped ones and a single repeat", got)
	}
}

func TestInterruptAfterTurnStartIsNotRepeated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, msgs, logPath := connectPickupStubClient(ctx, t)

	if err := client.Query(ctx, "start the turn"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	awaitMessage[*SystemMessage](t, msgs)

	if err := client.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	awaitStoppedTurn(t, msgs)

	if got := countInterrupts(stubStdinLog(t, logPath)); got != 1 {
		t.Errorf("CLI received %d interrupts, want only the one that was issued", got)
	}
}

// A turn that ends without ever announcing itself leaves nothing owed: the
// next turn's init must not inherit the repeat.
func TestPickupWindowClearsWhenTheTurnEndsWithoutStarting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, msgs, logPath := connectPickupStubClient(ctx, t)

	if err := client.Query(ctx, "noinit turn"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if err := client.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	awaitMessage[*ResultMessage](t, msgs)

	if err := client.Query(ctx, "start the turn"); err != nil {
		t.Fatalf("second Query: %v", err)
	}
	answer := awaitMessage[*AssistantMessage](t, msgs)
	if text, ok := answer.Content[0].(*TextBlock); !ok || text.Text != "full turn" {
		t.Errorf("second turn answered %#v, want the full answer", answer.Content)
	}
	awaitMessage[*ResultMessage](t, msgs)

	if got := countInterrupts(stubStdinLog(t, logPath)); got != 1 {
		t.Errorf("CLI received %d interrupts, want only the one that was issued", got)
	}
}
