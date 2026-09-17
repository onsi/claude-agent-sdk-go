package subprocess

import (
	"io"
	"sync"
)

// stdinWriter is the only way anything writes to the child's stdin. User
// messages and control-protocol frames are written from different goroutines,
// and the CLI reads stdin as JSON lines, so every Write must reach the pipe
// whole before the next one starts.
type stdinWriter struct {
	mu        sync.Mutex
	w         io.WriteCloser
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newStdinWriter(w io.WriteCloser) *stdinWriter {
	return &stdinWriter{w: w, closed: make(chan struct{})}
}

func (s *stdinWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	return s.w.Write(p)
}

// Close does not wait for a Write in progress: closing the pipe is what
// unblocks a Write stalled on a child that has stopped reading.
func (s *stdinWriter) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.closeErr = s.w.Close()
	})
	return s.closeErr
}
