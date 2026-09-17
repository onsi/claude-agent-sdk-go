package claudecode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/severity1/claude-agent-sdk-go/internal/cli"
	"github.com/severity1/claude-agent-sdk-go/internal/subprocess"
)

// ErrNoMoreMessages indicates the message iterator has no more messages.
var ErrNoMoreMessages = errors.New("no more messages")

// Query executes a one-shot query with automatic cleanup.
// This follows the Python SDK pattern but uses dependency injection for transport.
//
// Permission callbacks (WithCanUseTool), hooks and SDK MCP servers are
// honoured as they are by Client: with any of them set, the CLI runs in
// streaming input mode, the prompt is written to its stdin after the control
// handshake, and stdin is closed once the ResultMessage arrives. Otherwise the
// prompt is passed on the command line with --print.
//
// The CLI process starts on the first call to Next. Once Next returns a
// non-nil error — ErrNoMoreMessages when the stream ends cleanly, a
// *ProcessError when the CLI exited with a non-zero status or a signal, or any
// other terminal error — the iterator has already closed its transport,
// reaping the process and removing its temporary files. Call Close to release
// them when abandoning the iterator before that point; Close is safe to call
// in every case.
func Query(ctx context.Context, prompt string, opts ...Option) (MessageIterator, error) {
	options := NewOptions(opts...)
	if err := prepareOptions(options); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	transport, err := createQueryTransport(prompt, options)
	if err != nil {
		return nil, fmt.Errorf("failed to create query transport: %w", err)
	}

	return queryWithTransportAndOptions(ctx, prompt, transport, options)
}

// QueryWithTransport executes a query with a custom transport.
// The transport parameter is required and must not be nil.
func QueryWithTransport(
	ctx context.Context,
	prompt string,
	transport Transport,
	opts ...Option,
) (MessageIterator, error) {
	if transport == nil {
		return nil, fmt.Errorf("transport is required")
	}

	options := NewOptions(opts...)
	return queryWithTransportAndOptions(ctx, prompt, transport, options)
}

// Internal helper functions
func queryWithTransportAndOptions(
	ctx context.Context,
	prompt string,
	transport Transport,
	options *Options,
) (MessageIterator, error) {
	if transport == nil {
		return nil, fmt.Errorf("transport is required")
	}

	// Create iterator that manages the transport lifecycle
	return &queryIterator{
		transport: transport,
		prompt:    prompt,
		ctx:       ctx,
		options:   options,
	}, nil
}

// queryIterator implements MessageIterator for simple queries
type queryIterator struct {
	transport Transport
	prompt    string
	ctx       context.Context
	options   *Options
	started   bool
	msgChan   <-chan Message
	errChan   <-chan error
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
}

func (qi *queryIterator) Next(_ context.Context) (Message, error) {
	qi.mu.Lock()
	if qi.closed {
		qi.mu.Unlock()
		return nil, ErrNoMoreMessages
	}

	// Initialize on first call
	if !qi.started {
		qi.started = true
		if err := qi.start(); err != nil {
			qi.mu.Unlock()
			return nil, qi.finish(err)
		}
	}
	qi.mu.Unlock()

	for {
		select {
		case msg, ok := <-qi.msgChan:
			if ok {
				return msg, nil
			}
			return nil, qi.finish(exitFailure(qi.transport))
		case err, ok := <-qi.errChan:
			if !ok {
				// The error channel closes just before the message channel,
				// which may still hold buffered messages.
				qi.errChan = nil
				continue
			}
			return nil, qi.finish(err)
		case <-qi.ctx.Done():
			return nil, qi.finish(qi.ctx.Err())
		}
	}
}

// finish releases the transport and returns err, or ErrNoMoreMessages when
// neither err nor closing the transport produced an error.
func (qi *queryIterator) finish(err error) error {
	closeErr := qi.Close()
	switch {
	case err != nil:
		return err
	case closeErr != nil:
		return closeErr
	default:
		return ErrNoMoreMessages
	}
}

func (qi *queryIterator) Close() error {
	var err error
	qi.closeOnce.Do(func() {
		qi.mu.Lock()
		qi.closed = true
		qi.mu.Unlock()
		if qi.transport != nil {
			err = qi.transport.Close()
		}
	})
	return err
}

func (qi *queryIterator) start() error {
	// Connect to transport
	if err := qi.transport.Connect(qi.ctx); err != nil {
		return fmt.Errorf("failed to connect transport: %w", err)
	}

	// Get message channels
	msgChan, errChan := qi.transport.ReceiveMessages(qi.ctx)
	qi.msgChan = msgChan
	qi.errChan = errChan

	// Send the prompt
	userMsg := &UserMessage{Content: qi.prompt}
	streamMsg := StreamMessage{
		Type:    "request",
		Message: userMsg,
	}

	if err := qi.transport.SendMessage(qi.ctx, streamMsg); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// createQueryTransport creates a transport for a one-shot query.
//
// If the caller supplied a CLI path via WithCLIPath, that path is used directly
// and CLI auto-discovery is skipped. This matches the documented behaviour of
// the option (and the Python SDK's `cli_path` parameter) and lets callers
// bypass exec.LookPath when, for example, the npm shim on their platform
// mishandles command-line arguments.
func createQueryTransport(prompt string, options *Options) (Transport, error) {
	cliPath, err := resolveCLIPath(options)
	if err != nil {
		return nil, err
	}

	return subprocess.NewWithPrompt(cliPath, options, prompt), nil
}

// resolveCLIPath returns the CLI path the transport should invoke. When
// options.CLIPath is set and non-empty, it wins over auto-discovery — the
// caller has explicitly opted out of FindCLI's PATH/well-known-location
// search.
func resolveCLIPath(options *Options) (string, error) {
	if options != nil && options.CLIPath != nil && *options.CLIPath != "" {
		return *options.CLIPath, nil
	}
	return cli.FindCLI()
}
