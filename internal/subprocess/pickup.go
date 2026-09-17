package subprocess

import "sync"

// pickup tracks the window between writing a user message and the CLI
// announcing the turn with its init message. A CLI that has not yet picked the
// message up finds itself idle when an interrupt arrives, drops it, and then
// runs the queued turn in full — so a stop issued inside that window is owed a
// repeat once the turn starts.
type pickup struct {
	mu      sync.Mutex
	gen     uint64
	pending bool
	again   bool
}

// dispatched opens the window, or keeps an open one: a stream that arms on
// entry and again on its first write is one turn. The returned generation
// identifies this arming.
func (p *pickup) dispatched() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pending {
		p.pending, p.again = true, false
	}
	p.gen++
	return p.gen
}

// abandoned closes a window that never carried a message. Any later arming
// supersedes it, and then this does nothing.
func (p *pickup) abandoned(gen uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen == gen {
		p.pending, p.again = false, false
	}
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
