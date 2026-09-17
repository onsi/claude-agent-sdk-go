// Package subprocess provides the subprocess transport implementation for Claude Code CLI.
package subprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/severity1/claude-agent-sdk-go/internal/cli"
	"github.com/severity1/claude-agent-sdk-go/internal/control"
	"github.com/severity1/claude-agent-sdk-go/internal/parser"
	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

const (
	// channelBufferSize is the buffer size for message and error channels.
	channelBufferSize = 10
	// terminationTimeoutSeconds is the timeout for graceful process termination.
	terminationTimeoutSeconds = 5
	// windowsOS is the GOOS value for Windows platform.
	windowsOS = "windows"
)

// Transport implements the Transport interface using subprocess communication.
type Transport struct {
	// Process management
	cmd         *exec.Cmd
	cliPath     string
	options     *shared.Options
	closeStdin  bool
	promptArg   *string // For one-shot queries, prompt passed as CLI argument
	queryPrompt *string // For one-shot queries, prompt written to stdin after the control handshake
	entrypoint  string  // CLAUDE_CODE_ENTRYPOINT value (sdk-go or sdk-go-client)

	// proc is guarded by procMu as well as mu so Done and Err never wait on a
	// Close that holds mu while the child terminates.
	proc   *process
	procMu sync.RWMutex

	// Connection state
	connected bool
	mu        sync.RWMutex
	// handshakeDone mirrors connected for the stdout reader, which must not
	// take mu: Connect holds it while waiting for the reader to route the
	// handshake response.
	handshakeDone int32

	// I/O streams
	stdin      *stdinWriter
	stdout     *os.File
	stderr     *os.File // Temporary file for stderr isolation
	stderrPipe *os.File // Pipe for callback-based stderr handling

	// childPipeEnds are the write ends handed to the child, closed in the parent once it starts.
	childPipeEnds []*os.File

	// Temporary files (cleaned up on Close)
	mcpConfigFile *os.File // Temporary MCP config file

	// pickup arms the repeat of an interrupt the CLI dropped as idle.
	pickup pickup

	// Message parsing
	parser *parser.Parser

	// Stream validation
	validator *shared.StreamValidator

	// Channels for communication
	msgChan chan shared.Message
	errChan chan error

	// Control protocol (for streaming mode only)
	protocol        *control.Protocol
	protocolAdapter *ProtocolAdapter

	// Control and cleanup
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new subprocess transport.
func New(cliPath string, options *shared.Options, closeStdin bool, entrypoint string) *Transport {
	return &Transport{
		cliPath:    cliPath,
		options:    options,
		closeStdin: closeStdin,
		entrypoint: entrypoint,
		parser:     newParser(options),
		validator:  shared.NewStreamValidator(),
	}
}

// NewWithPrompt creates a new subprocess transport for a one-shot query.
// The prompt is passed as a CLI argument, unless options carry a permission
// callback, hooks or SDK MCP servers: the CLI can only consult those over the
// control protocol, so the prompt is then written to stdin in streaming input
// mode and stdin is closed once the result arrives.
func NewWithPrompt(cliPath string, options *shared.Options, prompt string) *Transport {
	t := &Transport{
		cliPath:    cliPath,
		options:    options,
		closeStdin: true,
		entrypoint: "sdk-go", // Query mode uses sdk-go
		parser:     newParser(options),
		validator:  shared.NewStreamValidator(),
	}
	if needsControlCallbacks(options) {
		t.queryPrompt = &prompt
	} else {
		t.promptArg = &prompt
	}
	return t
}

// newParser creates a parser using the buffer size from options, or the default.
func newParser(options *shared.Options) *parser.Parser {
	if options != nil && options.MaxBufferSize != nil {
		return parser.NewWithSize(*options.MaxBufferSize)
	}
	return parser.New()
}

// streamingInput reports whether the CLI reads stream-json from stdin and so
// can speak the control protocol.
func (t *Transport) streamingInput() bool {
	return !t.closeStdin || t.queryPrompt != nil
}

// IsConnected returns whether the transport is currently connected.
func (t *Transport) IsConnected() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.connected && t.cmd != nil && t.cmd.Process != nil && !t.proc.exited()
}

// Done returns a channel that is closed once the CLI process has exited and
// been reaped, whether it exited on its own or was ended by Close. Before the
// first Connect it returns an already-closed channel.
func (t *Transport) Done() <-chan struct{} {
	t.procMu.RLock()
	defer t.procMu.RUnlock()
	if t.proc == nil {
		return alreadyDone
	}
	return t.proc.done
}

// Err returns nil while the CLI process is running. Once Done is closed it
// returns a non-nil error: a *shared.ProcessError carrying the exit code (-1
// for a signal) when the process exited on its own, or an error stating the
// transport was closed when Close ended it or no process was ever started.
func (t *Transport) Err() error {
	t.procMu.RLock()
	defer t.procMu.RUnlock()
	if t.proc == nil {
		return errTransportClosed
	}
	return t.proc.err()
}

// Connect starts the Claude CLI subprocess.
func (t *Transport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.connected {
		return fmt.Errorf("transport already connected")
	}
	atomic.StoreInt32(&t.handshakeDone, 0)

	// Generate MCP config file if McpServers are specified
	opts, err := t.prepareMcpConfig()
	if err != nil {
		return err
	}

	args := t.buildArgs(opts)
	//nolint:gosec // G204: This is the core CLI SDK functionality - subprocess execution is required
	t.cmd = exec.CommandContext(ctx, args[0], args[1:]...)

	// Set up environment and apply to command
	t.cmd.Env = t.buildEnvironment()

	// Set working directory if specified
	if t.options != nil && t.options.Cwd != nil {
		if err := cli.ValidateWorkingDirectory(*t.options.Cwd); err != nil {
			return err
		}
		t.cmd.Dir = *t.options.Cwd
	}

	// Check CLI version and warn if outdated (non-blocking)
	t.emitCLIVersionWarning(ctx)

	// Set up I/O pipes
	if err := t.setupIoPipes(); err != nil {
		t.cleanup()
		return err
	}

	// Start the process
	if err := t.cmd.Start(); err != nil {
		t.cleanup()
		return shared.NewConnectionError(
			fmt.Sprintf("failed to start Claude CLI: %v", err),
			err,
		)
	}

	t.procMu.Lock()
	t.proc = watchProcess(t.cmd)
	t.procMu.Unlock()
	t.closeChildPipeEnds()

	// Set up context for goroutine management
	t.ctx, t.cancel = context.WithCancel(ctx)

	// Initialize channels
	t.msgChan = make(chan shared.Message, channelBufferSize)
	t.errChan = make(chan error, channelBufferSize)

	if t.streamingInput() {
		t.protocolAdapter = NewProtocolAdapter(t.stdin)
		t.protocol = control.NewProtocol(t.protocolAdapter, t.buildProtocolOptions()...)
	}

	// Start I/O handling goroutines
	t.wg.Add(1)
	go t.handleStdout(t.stdout, t.proc)

	// Start stderr callback goroutine if callback is configured
	if t.stderrPipe != nil && t.options != nil && t.options.StderrCallback != nil {
		t.wg.Add(1)
		go t.handleStderrCallback(t.stderrPipe)
	}

	if err := t.setupControlProtocol(t.ctx); err != nil {
		_ = t.shutdown()
		return err
	}

	t.connected = true
	atomic.StoreInt32(&t.handshakeDone, 1)
	return nil
}

func (t *Transport) buildArgs(opts *shared.Options) []string {
	if t.promptArg != nil {
		return cli.BuildCommandWithPrompt(t.cliPath, opts, *t.promptArg)
	}
	return cli.BuildCommand(t.cliPath, opts, !t.streamingInput())
}

// queryUserMessage is the stream-json user message carrying a one-shot prompt.
func queryUserMessage(prompt string) shared.StreamMessage {
	return shared.StreamMessage{
		Type:      "user",
		Message:   map[string]any{"role": "user", "content": prompt},
		SessionID: "default",
	}
}

// setupControlProtocol starts the control protocol, when the CLI reads
// streaming input, performs the handshake if any option needs it, and then
// writes a one-shot query's prompt.
func (t *Transport) setupControlProtocol(ctx context.Context) error {
	if t.protocol == nil {
		return nil
	}

	if err := t.protocol.Start(ctx); err != nil {
		return fmt.Errorf("failed to start control protocol: %w", err)
	}

	// Perform handshake when hooks, permissions, checkpointing, or SDK MCP servers configured
	if t.needsProtocolHandshake() {
		if _, err := t.protocol.Initialize(ctx); err != nil {
			return fmt.Errorf("failed to initialize control protocol: %w", err)
		}
	}

	if t.queryPrompt != nil {
		return t.writeMessage(t.stdin, queryUserMessage(*t.queryPrompt))
	}
	return nil
}

// needsProtocolHandshake returns true if control protocol handshake is required.
func (t *Transport) needsProtocolHandshake() bool {
	if t.options == nil {
		return false
	}
	return needsControlCallbacks(t.options) || t.options.EnableFileCheckpointing
}

// needsControlCallbacks reports whether options carry anything the CLI calls
// back into over the control protocol.
func needsControlCallbacks(options *shared.Options) bool {
	if options == nil {
		return false
	}
	return options.Hooks != nil ||
		options.CanUseTool != nil ||
		hasSdkMcpServers(options)
}

// SendMessage sends a message to the CLI subprocess.
func (t *Transport) SendMessage(ctx context.Context, message shared.StreamMessage) error {
	// A one-shot query's prompt is already on the command line or was written
	// by Connect.
	if t.promptArg != nil || t.queryPrompt != nil {
		return nil
	}

	t.mu.RLock()
	connected, stdin, proc := t.connected, t.stdin, t.proc
	t.mu.RUnlock()

	if !connected || stdin == nil {
		return fmt.Errorf("transport not connected or stdin closed")
	}

	if err := proc.err(); err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	// Check context cancellation
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if err := t.writeMessage(stdin, message); err != nil {
		return err
	}

	if t.closeStdin {
		_ = stdin.Close()
	}

	return nil
}

// writeMessage writes message to stdin as one JSON line. A user message opens
// the pickup window before it is written, since the CLI can read it the moment
// it lands.
func (t *Transport) writeMessage(stdin *stdinWriter, message shared.StreamMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	var gen uint64
	if message.Type == "user" && t.streamingInput() {
		gen = t.pickup.dispatched()
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		t.pickup.abandoned(gen)
		return fmt.Errorf("failed to write message: %w", err)
	}
	return nil
}

// DispatchTurn opens the pickup window for a turn whose user message has not
// been written yet, such as one handed to an asynchronous writer. The returned
// function closes that window again if no message followed.
func (t *Transport) DispatchTurn() func() {
	if !t.streamingInput() {
		return func() {}
	}
	gen := t.pickup.dispatched()
	return func() { t.pickup.abandoned(gen) }
}

// ReceiveMessages returns channels for receiving messages and errors.
func (t *Transport) ReceiveMessages(_ context.Context) (<-chan shared.Message, <-chan error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if !t.connected {
		// Return closed channels if not connected
		msgChan := make(chan shared.Message)
		errChan := make(chan error)
		close(msgChan)
		close(errChan)
		return msgChan, errChan
	}

	return t.msgChan, t.errChan
}

// Close terminates the subprocess connection.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.connected {
		return nil // Already closed
	}

	t.connected = false
	return t.shutdown()
}
