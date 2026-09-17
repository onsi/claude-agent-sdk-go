package subprocess

import "sync"

// pickup tracks the window between writing a user message and the CLI
// announcing the turn with its init message. A CLI that has not yet picked the
// message up finds itself idle when an interrupt arrives, drops it, and then
// runs the queued turn in full — so a stop issued inside that window is owed a
// repeat once the turn starts.
type pickup struct {
	mu      sync.Mutex
	pending bool
	again   bool
}

func (p *pickup) dispatched() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending, p.again = true, false
}

func (p *pickup) stopped() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending {
		p.again = true
	}
}

// started closes the window and reports whether a stop is owed to the turn
// that just began.
func (p *pickup) started() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	again := p.again
	p.pending, p.again = false, false
	return again
}

func (p *pickup) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending, p.again = false, false
}
