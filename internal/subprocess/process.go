package subprocess

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

var errTransportClosed = errors.New("transport closed")

var alreadyDone = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// process tracks one started CLI child. Its waiter goroutine is the only caller
// of cmd.Wait: a second concurrent Wait is an error in os/exec and would race
// the first for the exit status.
type process struct {
	done    chan struct{}
	waitErr error

	mu     sync.Mutex
	closed bool
}

func watchProcess(cmd *exec.Cmd) *process {
	p := &process{done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// markClosed records that Close, not the child, ended the process. It is a
// no-op once the child has exited so that err never changes after it is first
// non-nil.
func (p *process) markClosed() {
	p.mu.Lock()
	if !p.exited() {
		p.closed = true
	}
	p.mu.Unlock()
}

// err returns nil while the child runs, and a non-nil reason once it has been reaped.
func (p *process) err() error {
	if !p.exited() {
		return nil
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errTransportClosed
	}
	if p.waitErr == nil {
		return shared.NewProcessError("claude process exited", 0, "")
	}
	var exitErr *exec.ExitError
	if errors.As(p.waitErr, &exitErr) && exitErr.Exited() {
		return shared.NewProcessError("claude process exited", exitErr.ExitCode(), "")
	}
	return shared.NewProcessError(fmt.Sprintf("claude process exited: %v", p.waitErr), -1, "")
}

// isProcessAlreadyFinishedError checks if an error indicates the process has already terminated.
// This follows the Python SDK pattern of suppressing "process not found" type errors.
func isProcessAlreadyFinishedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "process already finished") ||
		strings.Contains(errStr, "process already released") ||
		strings.Contains(errStr, "no child processes") ||
		strings.Contains(errStr, "signal: killed")
}

// terminateProcess asks the child to exit, waits up to the termination timeout,
// then kills it. In streaming mode the request is the stdin EOF the caller has
// already delivered, so the CLI can flush its session before exiting; one-shot
// mode gets SIGTERM instead. It returns only once the child has been reaped.
func (t *Transport) terminateProcess(p *process) error {
	if p == nil {
		return nil
	}
	p.markClosed()
	if p.exited() {
		return nil
	}

	if t.closeStdin {
		if err := t.cmd.Process.Signal(syscall.SIGTERM); err != nil && !isProcessAlreadyFinishedError(err) {
			return t.killProcess(p)
		}
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(terminationTimeoutSeconds * time.Second):
		return t.killProcess(p)
	}
}

func (t *Transport) killProcess(p *process) error {
	if err := t.cmd.Process.Kill(); err != nil && !isProcessAlreadyFinishedError(err) {
		return err
	}
	<-p.done
	return nil
}

// shutdown stops a started child and every goroutine reading from it, then
// releases all resources. The caller holds t.mu.
func (t *Transport) shutdown() error {
	if t.protocol != nil {
		_ = t.protocol.Close()
	}
	if t.protocolAdapter != nil {
		_ = t.protocolAdapter.Close()
	}
	if t.stdin != nil {
		_ = t.stdin.Close()
	}

	err := t.terminateProcess(t.proc)

	if t.cancel != nil {
		t.cancel()
	}
	// A grandchild that inherited the child's stdout or stderr can hold the pipe
	// open past the child's exit; closing our read ends unblocks the readers.
	if t.stdout != nil {
		_ = t.stdout.Close()
	}
	if t.stderrPipe != nil {
		_ = t.stderrPipe.Close()
	}
	t.wg.Wait()

	t.cleanup()
	return err
}

// cleanup cleans up all resources
func (t *Transport) cleanup() {
	t.closeChildPipeEnds()

	if t.stdout != nil {
		_ = t.stdout.Close()
		t.stdout = nil
	}

	if t.stderrPipe != nil {
		_ = t.stderrPipe.Close()
		t.stderrPipe = nil
	}

	if t.stderr != nil {
		// Graceful cleanup matching Python SDK pattern
		// Python: except Exception: pass
		_ = t.stderr.Close()
		_ = os.Remove(t.stderr.Name()) // Ignore cleanup errors
		t.stderr = nil
	}

	if t.mcpConfigFile != nil {
		// Clean up temporary MCP config file
		_ = t.mcpConfigFile.Close()
		_ = os.Remove(t.mcpConfigFile.Name()) // Ignore cleanup errors
		t.mcpConfigFile = nil
	}

	t.protocol = nil
	t.protocolAdapter = nil
	t.stdin = nil
	t.pickup.reset()

	// Reset state
	t.cmd = nil
}
