package subprocess

import (
	"testing"

	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

func TestPickupOwesARepeatForAStopInsideTheWindow(t *testing.T) {
	var p pickup
	p.dispatched()
	p.stopped()
	if !p.started() {
		t.Error("a stop issued inside the window owes a repeat")
	}
	if p.started() {
		t.Error("the repeat is owed only once")
	}
}

func TestPickupOwesNothingForAStopOutsideTheWindow(t *testing.T) {
	var p pickup
	p.stopped()
	if p.started() {
		t.Error("a stop issued with no turn queued owes a repeat")
	}
}

func TestPickupAbandonedWindowOwesNothing(t *testing.T) {
	var p pickup
	gen := p.dispatched()
	p.stopped()
	p.abandoned(gen)
	if p.started() {
		t.Error("a window nothing was ever written to still owes a repeat")
	}
}

// A stream arms on entry and again when it writes; that is one window, and the
// stream ending afterwards must not close it.
func TestPickupWriteAfterArmingKeepsTheWindow(t *testing.T) {
	var p pickup
	stream := p.dispatched()
	p.stopped()
	p.dispatched()
	p.abandoned(stream)
	if !p.started() {
		t.Error("the write's window was closed by the stream that armed it")
	}
}

func TestPickupResetClosesTheWindow(t *testing.T) {
	var p pickup
	p.dispatched()
	p.stopped()
	p.reset()
	if p.started() {
		t.Error("a reset window still owes a repeat")
	}
}

func TestDispatchTurnReleaseClosesTheWindow(t *testing.T) {
	transport := New("/usr/bin/claude", &shared.Options{}, false, "sdk-go-client")

	release := transport.DispatchTurn()
	transport.pickup.stopped()
	release()

	if transport.pickup.started() {
		t.Error("releasing a dispatch left a repeat owed")
	}
}

func TestDispatchTurnIsInertWithoutStreamingInput(t *testing.T) {
	transport := NewWithPrompt("/usr/bin/claude", &shared.Options{}, "hello")

	transport.DispatchTurn()
	transport.pickup.stopped()

	if transport.pickup.started() {
		t.Error("a one-shot transport opened a pickup window")
	}
}
