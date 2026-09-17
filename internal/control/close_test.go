package control

import (
	"errors"
	"testing"
	"time"
)

func pendingRequestCount(p *Protocol) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pendingRequests)
}

func TestCloseUnblocksPendingRequest(t *testing.T) {
	ctx, _, protocol := startTaskTestProtocol(t)

	done := make(chan error, 1)
	go func() { done <- protocol.Interrupt(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for pendingRequestCount(protocol) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("interrupt request never became pending")
		}
		time.Sleep(time.Millisecond)
	}

	if err := protocol.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrProtocolClosed) {
			t.Errorf("Interrupt = %v, want ErrProtocolClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Interrupt still blocked after Close")
	}
}

func TestRequestAfterCloseFailsImmediately(t *testing.T) {
	ctx, transport, protocol := startTaskTestProtocol(t)
	if err := protocol.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	start := time.Now()
	if err := protocol.Interrupt(ctx); !errors.Is(err, ErrProtocolClosed) {
		t.Errorf("Interrupt after Close = %v, want ErrProtocolClosed", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Interrupt after Close took %v", elapsed)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.writtenData) != 0 {
		t.Errorf("a request was written after Close: %q", transport.writtenData)
	}
}
