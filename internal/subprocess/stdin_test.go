package subprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/severity1/claude-agent-sdk-go/internal/control"
	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

// stallingPipe records what is written to it. The first Write stops halfway
// until a second Write starts or a grace period passes, so two unserialized
// writers always interleave and serialized ones never do.
type stallingPipe struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	writes      int
	active      int
	overlapped  bool
	secondEnter chan struct{}
	firstHalf   chan struct{}
}

func newStallingPipe() *stallingPipe {
	return &stallingPipe{secondEnter: make(chan struct{}), firstHalf: make(chan struct{})}
}

func (p *stallingPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.writes++
	n := p.writes
	p.active++
	if p.active > 1 {
		p.overlapped = true
	}
	if n == 2 {
		close(p.secondEnter)
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}()

	half := len(b) / 2
	p.append(b[:half])
	if n == 1 {
		close(p.firstHalf)
		select {
		case <-p.secondEnter:
			time.Sleep(10 * time.Millisecond)
		case <-time.After(200 * time.Millisecond):
		}
	}
	p.append(b[half:])
	return len(b), nil
}

func (p *stallingPipe) append(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
}

func (p *stallingPipe) Close() error { return nil }

func TestUserMessagesAndControlRequestsShareOneWritePath(t *testing.T) {
	pipe := newStallingPipe()
	transport := &Transport{
		connected: true,
		proc:      &process{done: make(chan struct{})},
		stdin:     newStdinWriter(pipe),
		options:   &shared.Options{},
	}
	transport.protocolAdapter = NewProtocolAdapter(transport.stdin)
	transport.protocol = control.NewProtocol(transport.protocolAdapter)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.protocol.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = transport.protocol.Close() }()

	sent := make(chan error, 1)
	go func() {
		sent <- transport.SendMessage(ctx, shared.StreamMessage{
			Type:    "user",
			Message: map[string]any{"role": "user", "content": strings.Repeat("x", 1<<16)},
		})
	}()
	<-pipe.firstHalf

	interruptCtx, cancelInterrupt := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelInterrupt()
	_ = transport.Interrupt(interruptCtx)

	if err := <-sent; err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	pipe.mu.Lock()
	defer pipe.mu.Unlock()
	if pipe.overlapped {
		t.Error("a control request was written while a user message was still being written")
	}
	lines := strings.Split(strings.TrimSuffix(pipe.buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdin carried %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("stdin line is not valid JSON: %.80q...", line)
		}
	}
}

func TestStdinWriterCloseRejectsLaterWrites(t *testing.T) {
	w := newStdinWriter(newStallingPipe())
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.Write([]byte("{}\n")); err == nil {
		t.Error("Write after Close succeeded")
	}
}
