package subprocess

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"github.com/severity1/claude-agent-sdk-go/internal/parser"
	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

// handleStdout processes stdout in a separate goroutine. After a clean EOF it
// keeps the channels open until the child has been reaped, so a consumer that
// sees them close can read the exit status from Err.
func (t *Transport) handleStdout(stdout io.Reader, proc *process) {
	defer t.wg.Done()
	defer close(t.msgChan)
	defer close(t.errChan)
	defer t.validator.MarkStreamEnd() // Mark stream end for validation

	scanner := bufio.NewScanner(stdout)

	// Scanner token size must match the parser's buffer limit so lines aren't
	// truncated before parsing. Default is 64KB; respect MaxBufferSize if set.
	scanTokenSize := parser.MaxBufferSize
	if t.options != nil && t.options.MaxBufferSize != nil {
		scanTokenSize = *t.options.MaxBufferSize
	}
	buf := make([]byte, scanTokenSize)
	scanner.Buffer(buf, scanTokenSize)

	for scanner.Scan() {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		// Parse line with the parser
		messages, err := t.parser.ProcessLine(line)
		if err != nil {
			select {
			case t.errChan <- err:
			case <-t.ctx.Done():
				return
			}
			continue
		}

		// Send parsed messages and track for validation
		for _, msg := range messages {
			if msg == nil {
				continue
			}

			// If this is an error ResultMessage before we're fully connected,
			// it means the CLI failed during init (e.g., invalid session ID).
			// Route the error to the control protocol to unblock Initialize().
			t.routeInitError(msg)

			// Check if this is a control message that should be routed to the protocol
			if rawCtrl, ok := msg.(*shared.RawControlMessage); ok {
				// Route control messages to the protocol for request/response correlation
				if t.protocol != nil {
					// HandleIncomingMessage routes control responses to pending requests
					// and forwards non-control messages to the protocol's message stream
					_ = t.protocol.HandleIncomingMessage(t.ctx, rawCtrl.Data)
				}
				// Don't send control messages to msgChan - they're internal to the protocol
				continue
			}

			// Track regular message for stream validation
			t.validator.TrackMessage(msg)
			t.endQueryInput(msg)
			t.notePickup(msg)

			select {
			case t.msgChan <- msg:
			case <-t.ctx.Done():
				return
			}
		}
	}

	t.endStdout(scanner.Err(), proc)
}

// endStdout reports a scanner failure, or after a clean EOF waits for the
// child to be reaped.
func (t *Transport) endStdout(scanErr error, proc *process) {
	if scanErr != nil {
		select {
		case t.errChan <- fmt.Errorf("stdout scanner error: %w", scanErr):
		case <-t.ctx.Done():
		}
		return
	}
	select {
	case <-proc.done:
	case <-t.ctx.Done():
	}
}

// handleStderrCallback processes stderr in a separate goroutine.
// Reads line-by-line, strips trailing whitespace, skips empty lines, and
// silently ignores scanner errors.
func (t *Transport) handleStderrCallback(stderr io.Reader) {
	defer t.wg.Done()

	scanner := bufio.NewScanner(stderr)

	for scanner.Scan() {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		// Strip trailing whitespace (matches Python's rstrip())
		line := strings.TrimRight(scanner.Text(), " \t\r\n")

		// Skip empty lines (matches Python SDK behavior)
		if line == "" {
			continue
		}

		// Call the callback synchronously (matches Python SDK)
		// Recover from panics to prevent crashing the SDK
		func() {
			defer func() {
				_ = recover() // Silently ignore callback panics (matches Python's pass)
			}()
			t.options.StderrCallback(line)
		}()
	}
	// Silently ignore scanner errors (matches Python SDK's except Exception: pass)
}

// endQueryInput closes stdin once a one-shot query's result arrives, which
// is what makes a CLI in streaming input mode exit.
func (t *Transport) endQueryInput(msg shared.Message) {
	if t.queryPrompt == nil {
		return
	}
	if _, ok := msg.(*shared.ResultMessage); ok {
		_ = t.stdin.Close()
	}
}

// notePickup follows the turn the CLI is running: its init message closes the
// pickup window and repeats an interrupt dropped inside it, and a result ends
// the turn whether or not one ever started.
func (t *Transport) notePickup(msg shared.Message) {
	switch m := msg.(type) {
	case *shared.SystemMessage:
		if m.Subtype != "init" {
			return
		}
		if t.pickup.started() {
			go t.repeatInterrupt(t.ctx)
		}
	case *shared.ResultMessage:
		t.pickup.reset()
	}
}

// routeInitError checks if a message is an error ResultMessage arriving before
// the transport is fully connected, and routes it to the control protocol to
// unblock Initialize().
func (t *Transport) routeInitError(msg shared.Message) {
	resultMsg, ok := msg.(*shared.ResultMessage)
	if !ok || atomic.LoadInt32(&t.handshakeDone) == 1 || !resultMsg.IsError || t.protocol == nil {
		return
	}
	t.protocol.HandleControlInitErr(errors.New(formatInitError(resultMsg)))
}

// formatInitError builds a meaningful error string from a ResultMessage that
// arrived during initialization. Prefers Errors, falls back to Result, then Subtype.
func formatInitError(msg *shared.ResultMessage) string {
	if len(msg.Errors) > 0 {
		return strings.Join(msg.Errors, "; ")
	}
	if msg.Result != nil && *msg.Result != "" {
		return *msg.Result
	}
	return fmt.Sprintf("initialization failed with subtype: %s", msg.Subtype)
}

// setupStderr configures stderr handling based on options.
// Precedence: StderrCallback > DebugWriter > temp file (default).
func (t *Transport) setupStderr() error {
	switch {
	case t.options != nil && t.options.StderrCallback != nil:
		// Create pipe for callback-based stderr handling
		r, w, err := t.pipeFromChild()
		if err != nil {
			return fmt.Errorf("failed to create stderr pipe: %w", err)
		}
		t.stderrPipe = r
		t.cmd.Stderr = w
	case t.options != nil && t.options.DebugWriter != nil:
		// Use custom debug writer provided by user
		t.cmd.Stderr = t.options.DebugWriter
	default:
		// Isolate stderr using temporary file to prevent deadlocks
		// This matches Python SDK pattern to avoid subprocess pipe deadlocks
		stderrFile, err := os.CreateTemp("", "claude_stderr_*.log")
		if err != nil {
			return fmt.Errorf("failed to create stderr file: %w", err)
		}
		t.stderr = stderrFile
		t.cmd.Stderr = t.stderr
	}
	return nil
}

// setupIoPipes configures stdin, stdout, and stderr pipes for the subprocess.
// For streaming mode, creates a stdin pipe for sending messages. Always creates
// stdout pipe for receiving responses. Stderr is configured via setupStderr.
func (t *Transport) setupIoPipes() error {
	if t.promptArg == nil {
		// Only create stdin pipe if we need to send messages via stdin
		pipe, err := t.cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdin pipe: %w", err)
		}
		t.stdin = newStdinWriter(pipe)
	}

	r, w, err := t.pipeFromChild()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	t.stdout = r
	t.cmd.Stdout = w

	// Handle stderr configuration
	if err := t.setupStderr(); err != nil {
		return err
	}

	return nil
}

// pipeFromChild returns a new pipe whose write end is handed to the child. The
// os/exec StdoutPipe and StderrPipe helpers are unsuitable: Wait closes their
// read ends once the child exits, discarding unread output, and the process
// waiter calls Wait the moment the child exits.
func (t *Transport) pipeFromChild() (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	t.childPipeEnds = append(t.childPipeEnds, w)
	return r, w, nil
}

// closeChildPipeEnds closes the parent's copies of the child's write ends, so
// readers see EOF once the child (and anything it spawned) closes its copies.
func (t *Transport) closeChildPipeEnds() {
	for _, w := range t.childPipeEnds {
		_ = w.Close()
	}
	t.childPipeEnds = nil
}
