//go:build !windows

package claudecode

import (
	"context"
	"syscall"
	"testing"
	"time"
)

func connectLifecycleClient(t *testing.T, body string) (Client, int) {
	t.Helper()
	cliPath, pidFile := writeLifecycleCLI(t, body)
	client := NewClient(WithCLIPath(cliPath))
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect() })
	return client, readLifecyclePID(t, pidFile)
}

func awaitClosed(t *testing.T, msgs <-chan Message) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-msgs:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("message channel never closed")
		}
	}
}

func TestClientObservesChildDeath(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		kill     bool
		exitCode int
	}{
		{name: "killed", body: "exec cat >/dev/null", kill: true, exitCode: -1},
		{name: "exited cleanly", body: "exit 0", exitCode: 0},
		{name: "exited with failure", body: "exit 7", exitCode: 7},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, pid := connectLifecycleClient(t, tc.body)
			ctx := context.Background()
			msgs := client.ReceiveMessages(ctx)
			if tc.kill {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatalf("kill: %v", err)
				}
			}
			awaitClosed(t, msgs)

			select {
			case <-client.Done():
			default:
				t.Fatal("Done is still open after the message channel closed")
			}
			procErr := AsProcessError(client.Err())
			if procErr == nil || procErr.ExitCode != tc.exitCode {
				t.Fatalf("Err() = %v, want a ProcessError with exit code %d", client.Err(), tc.exitCode)
			}
			assertReaped(t, pid)

			if err := client.Query(ctx, "next turn"); !IsProcessError(err) {
				t.Errorf("Query after death = %v, want a ProcessError", err)
			}
			stream := make(chan StreamMessage)
			if err := client.QueryStream(ctx, stream); !IsProcessError(err) {
				t.Errorf("QueryStream after death = %v, want a ProcessError", err)
			}
			if err := client.Interrupt(ctx); !IsProcessError(err) {
				t.Errorf("Interrupt after death = %v, want a ProcessError", err)
			}

			if err := client.Disconnect(); err != nil {
				t.Errorf("Disconnect after death: %v", err)
			}
			if err := client.Disconnect(); err != nil {
				t.Errorf("second Disconnect: %v", err)
			}
		})
	}
}

func TestClientReceiveResponseReportsChildFailure(t *testing.T) {
	client, _ := connectLifecycleClient(t, "exit 5")
	iter := client.ReceiveResponse(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := iter.Next(ctx)
	if procErr := AsProcessError(err); procErr == nil || procErr.ExitCode != 5 {
		t.Fatalf("Next = %v, want a ProcessError with exit code 5", err)
	}
}

func TestClientDoneClosesOnDisconnect(t *testing.T) {
	client, pid := connectLifecycleClient(t, "exec cat >/dev/null")
	done := client.Done()
	if err := client.Err(); err != nil {
		t.Fatalf("Err while running = %v", err)
	}
	select {
	case <-done:
		t.Fatal("Done closed while the child is running")
	default:
	}

	if err := client.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Done did not close on Disconnect")
	}
	assertReaped(t, pid)
	if err := client.Err(); err == nil || IsProcessError(err) {
		t.Errorf("Err after Disconnect = %v, want a not-connected error", err)
	}
	select {
	case <-client.Done():
	default:
		t.Error("Done after Disconnect is not closed")
	}
}
