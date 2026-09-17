package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/severity1/claude-agent-sdk-go/internal/subprocess"
)

const defaultSessionID = "default"

// Client provides bidirectional streaming communication with Claude Code CLI.
type Client interface {
	Connect(ctx context.Context, prompt ...StreamMessage) error
	Disconnect() error
	Query(ctx context.Context, prompt string) error
	QueryWithSession(ctx context.Context, prompt string, sessionID string) error
	QueryStream(ctx context.Context, messages <-chan StreamMessage) error
	ReceiveMessages(ctx context.Context) <-chan Message
	ReceiveResponse(ctx context.Context) MessageIterator
	// Interrupt stops the current turn. The session stays connected and
	// accepts the next Query. Only works in streaming mode (after Connect()).
	Interrupt(ctx context.Context) error
	// StopTask stops one running task, such as a subagent, by the TaskID of
	// its TaskStartedMessage. The rest of the turn continues.
	// Only works in streaming mode (after Connect()).
	StopTask(ctx context.Context, taskID string) error
	// BackgroundTasks moves in-flight foreground tasks to the background so
	// the turn continues without waiting on them. A non-empty toolUseID
	// targets the task started by that tool_use block; empty targets all.
	// Only works in streaming mode (after Connect()).
	BackgroundTasks(ctx context.Context, toolUseID string) (bool, error)
	// SetModel changes the AI model during a streaming session.
	// Pass nil to reset to the default model.
	// Only works in streaming mode (after Connect()).
	SetModel(ctx context.Context, model *string) error
	// SetPermissionMode changes the permission mode during a streaming session.
	// Valid modes: PermissionModeDefault, PermissionModeAcceptEdits,
	// PermissionModePlan, PermissionModeBypassPermissions.
	// Only works in streaming mode (after Connect()).
	SetPermissionMode(ctx context.Context, mode PermissionMode) error
	// RewindFiles reverts tracked files to their state at a specific user message.
	// The messageUUID should be the UUID from a UserMessage received during the session.
	// Requires WithFileCheckpointing() or WithEnableFileCheckpointing(true) option.
	// Only works in streaming mode (after Connect()).
	RewindFiles(ctx context.Context, messageUUID string) error
	// GetMcpStatus returns the connection status of all configured MCP servers.
	// Only works in streaming mode (after Connect()).
	GetMcpStatus(ctx context.Context) (*McpStatusResponse, error)
	GetStreamIssues() []StreamIssue
	GetStreamStats() StreamStats
	GetServerInfo(ctx context.Context) (map[string]interface{}, error)
	// Done returns a channel that is closed once the connected CLI process has
	// exited, whether it exited on its own, was ended by Interrupt, or was
	// stopped by Disconnect; when the message channel closes because the
	// process ended, Done is already closed. Before Connect and after
	// Disconnect it returns an already-closed channel. With a custom transport
	// that cannot report its process, the channel closes on Disconnect.
	Done() <-chan struct{}
	// Err returns nil while the connected CLI process is running. Once Done is
	// closed it returns why the client can no longer serve a turn: a
	// *ProcessError (see AsProcessError) carrying the exit code, or -1 when a
	// signal ended the process, or a "client not connected" error before
	// Connect and after Disconnect. The same error is returned by every method
	// that would write to the process.
	Err() error
}

// processWatcher is implemented by transports that can report when their CLI
// process has exited.
type processWatcher interface {
	Done() <-chan struct{}
	Err() error
}

var errClientNotConnected = errors.New("client not connected")

var alreadyClosed = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// ClientImpl implements the Client interface.
type ClientImpl struct {
	mu              sync.RWMutex
	transport       Transport
	customTransport Transport // For testing with WithTransport
	options         *Options
	connected       bool
	msgChan         <-chan Message
	errChan         <-chan error
	streamErrChan   chan error // writable; receives errors from QueryStream goroutine
	disconnected    chan struct{}
}

// NewClient creates a new Client with the given options.
func NewClient(opts ...Option) Client {
	options := NewOptions(opts...)
	client := &ClientImpl{
		options: options,
	}
	return client
}

// NewClientWithTransport creates a new Client with a custom transport (for testing).
func NewClientWithTransport(transport Transport, opts ...Option) Client {
	options := NewOptions(opts...)
	return &ClientImpl{
		customTransport: transport,
		options:         options,
	}
}

// WithClient provides Go-idiomatic resource management equivalent to Python SDK's async context manager.
// It automatically connects to Claude Code CLI, executes the provided function, and ensures proper cleanup.
// This eliminates the need for manual Connect/Disconnect calls and prevents resource leaks.
//
// The function follows Go's established resource management patterns using defer for guaranteed cleanup,
// similar to how database connections, files, and other resources are typically managed in Go.
//
// Example - Basic usage:
//
//	err := claudecode.WithClient(ctx, func(client claudecode.Client) error {
//	    return client.Query(ctx, "What is 2+2?")
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
// Example - With configuration options:
//
//	err := claudecode.WithClient(ctx, func(client claudecode.Client) error {
//	    if err := client.Query(ctx, "Calculate the area of a circle with radius 5"); err != nil {
//	        return err
//	    }
//
//	    // Process responses
//	    for msg := range client.ReceiveMessages(ctx) {
//	        if assistantMsg, ok := msg.(*claudecode.AssistantMessage); ok {
//	            fmt.Println("Claude:", assistantMsg.Content[0].(*claudecode.TextBlock).Text)
//	        }
//	    }
//	    return nil
//	}, claudecode.WithSystemPrompt("You are a helpful math tutor"),
//	   claudecode.WithAllowedTools("Read", "Write"))
//
// The client will be automatically connected before fn is called and disconnected after fn returns,
// even if fn returns an error or panics. This provides 100% functional parity with Python SDK's
// 'async with ClaudeSDKClient()' pattern while using idiomatic Go resource management.
//
// Parameters:
//   - ctx: Context for connection management and cancellation
//   - fn: Function to execute with the connected client
//   - opts: Optional client configuration options
//
// Returns an error if connection fails or if fn returns an error.
// Disconnect errors are handled gracefully without overriding the original error from fn.
func WithClient(ctx context.Context, fn func(Client) error, opts ...Option) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	client := NewClient(opts...)

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect client: %w", err)
	}

	defer func() {
		// Following Go idiom: cleanup errors don't override the original error
		// This matches patterns in database/sql, os.File, and other stdlib packages
		if disconnectErr := client.Disconnect(); disconnectErr != nil {
			// Log cleanup errors but don't return them to preserve the original error
			// This follows the standard Go pattern for resource cleanup
			_ = disconnectErr // Explicitly acknowledge we're ignoring this error
		}
	}()

	return fn(client)
}

// WithClientTransport provides Go-idiomatic resource management with a custom transport for testing.
// This is the testing-friendly version of WithClient that accepts an explicit transport parameter.
//
// Usage in tests:
//
//	transport := newClientMockTransport()
//	err := WithClientTransport(ctx, transport, func(client claudecode.Client) error {
//	    return client.Query(ctx, "What is 2+2?")
//	}, opts...)
//
// Parameters:
//   - ctx: Context for connection management and cancellation
//   - transport: Custom transport to use (typically a mock for testing)
//   - fn: Function to execute with the connected client
//   - opts: Optional client configuration options
//
// Returns an error if connection fails or if fn returns an error.
// Disconnect errors are handled gracefully without overriding the original error from fn.
func WithClientTransport(ctx context.Context, transport Transport, fn func(Client) error, opts ...Option) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	client := NewClientWithTransport(transport, opts...)

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect client: %w", err)
	}

	defer func() {
		// Following Go idiom: cleanup errors don't override the original error
		if disconnectErr := client.Disconnect(); disconnectErr != nil {
			// Log cleanup errors but don't return them to preserve the original error
			_ = disconnectErr // Explicitly acknowledge we're ignoring this error
		}
	}()

	return fn(client)
}

// prepareOptions applies defaults and validates the client configuration options.
func (c *ClientImpl) prepareOptions() error {
	if c.options == nil {
		return nil // Nil options are acceptable (use defaults)
	}

	// Auto-configure PermissionPromptToolName when CanUseTool callback is set.
	// This tells CLI to route permission prompts through stdio (control protocol).
	if c.options.CanUseTool != nil && c.options.PermissionPromptToolName == nil {
		stdio := "stdio"
		c.options.PermissionPromptToolName = &stdio
	}

	// Validate working directory
	if c.options.Cwd != nil {
		if _, err := os.Stat(*c.options.Cwd); os.IsNotExist(err) {
			return fmt.Errorf("working directory does not exist: %s", *c.options.Cwd)
		}
	}

	// Validate max turns
	if c.options.MaxTurns < 0 {
		return fmt.Errorf("max_turns must be non-negative, got: %d", c.options.MaxTurns)
	}

	// Validate permission mode
	if c.options.PermissionMode != nil {
		validModes := map[PermissionMode]bool{
			PermissionModeDefault:           true,
			PermissionModeAcceptEdits:       true,
			PermissionModePlan:              true,
			PermissionModeBypassPermissions: true,
		}
		if !validModes[*c.options.PermissionMode] {
			return fmt.Errorf("invalid permission mode: %s", string(*c.options.PermissionMode))
		}
	}

	return nil
}

// Connect establishes a connection to the Claude Code CLI.
func (c *ClientImpl) Connect(ctx context.Context, _ ...StreamMessage) error {
	// Check context before acquiring lock
	if ctx.Err() != nil {
		return ctx.Err()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check context again after acquiring lock
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Validate configuration before connecting
	if err := c.prepareOptions(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Use custom transport if provided, otherwise create default
	if c.customTransport != nil {
		c.transport = c.customTransport
	} else {
		// Honor WithCLIPath when set, otherwise fall back to auto-discovery.
		cliPath, err := resolveCLIPath(c.options)
		if err != nil {
			return fmt.Errorf("claude CLI not found: %w", err)
		}

		// Create subprocess transport for streaming mode (closeStdin=false)
		c.transport = subprocess.New(cliPath, c.options, false, "sdk-go-client")
	}

	// Connect the transport
	if err := c.transport.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect transport: %w", err)
	}

	// Get message channels
	c.msgChan, c.errChan = c.transport.ReceiveMessages(ctx)
	c.streamErrChan = make(chan error, 1)
	c.disconnected = make(chan struct{})

	c.connected = true
	return nil
}

// Disconnect closes the connection to the Claude Code CLI.
func (c *ClientImpl) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.transport != nil && c.connected {
		if err := c.transport.Close(); err != nil {
			return fmt.Errorf("failed to close transport: %w", err)
		}
	}
	if c.connected {
		close(c.disconnected)
	}
	c.connected = false
	c.transport = nil
	c.msgChan = nil
	c.errChan = nil
	c.streamErrChan = nil
	return nil
}

// Query sends a simple text query using the default session.
// This is equivalent to QueryWithSession(ctx, prompt, "default").
//
// Example:
//
//	client.Query(ctx, "What is Go?")
func (c *ClientImpl) Query(ctx context.Context, prompt string) error {
	return c.queryWithSession(ctx, prompt, defaultSessionID)
}

// QueryWithSession sends a simple text query using the specified session ID.
// Each session maintains its own conversation context, allowing for isolated
// conversations within the same client connection.
//
// If sessionID is empty, it defaults to "default".
//
// Example:
//
//	client.QueryWithSession(ctx, "Remember this", "my-session")
//	client.QueryWithSession(ctx, "What did I just say?", "my-session") // Remembers context
//	client.Query(ctx, "What did I just say?")                          // Won't remember, different session
func (c *ClientImpl) QueryWithSession(ctx context.Context, prompt string, sessionID string) error {
	// Use default session if empty session ID provided
	if sessionID == "" {
		sessionID = defaultSessionID
	}
	return c.queryWithSession(ctx, prompt, sessionID)
}

// queryWithSession is the internal implementation for sending queries with session management.
func (c *ClientImpl) queryWithSession(ctx context.Context, prompt string, sessionID string) error {
	// Check context before proceeding
	if ctx.Err() != nil {
		return ctx.Err()
	}

	transport, err := c.liveTransport()
	if err != nil {
		return err
	}

	// Check context again after acquiring connection info
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Create user message in Python SDK compatible format
	streamMsg := StreamMessage{
		Type: "user",
		Message: map[string]interface{}{
			"role":    "user",
			"content": prompt,
		},
		ParentToolUseID: nil,
		SessionID:       sessionID,
	}

	// Send message via transport (without holding mutex to avoid blocking other operations)
	return transport.SendMessage(ctx, streamMsg)
}

// QueryStream sends each message received from messages until that channel
// closes or ctx is done. It returns an error only when the client cannot write
// at all (see Err); the sends happen asynchronously, so a later send failure is
// reported by the iterator from ReceiveResponse, and a send that failed because
// the CLI process exited is also visible through Done and Err.
func (c *ClientImpl) QueryStream(ctx context.Context, messages <-chan StreamMessage) error {
	transport, err := c.liveTransport()
	if err != nil {
		return err
	}
	c.mu.RLock()
	streamErrChan := c.streamErrChan
	c.mu.RUnlock()

	// Send messages from channel in a goroutine
	go func() {
		for {
			select {
			case msg, ok := <-messages:
				if !ok {
					return // Channel closed
				}
				if err := transport.SendMessage(ctx, msg); err != nil {
					fmt.Fprintf(os.Stderr, "claude-agent-sdk: QueryStream send error: %v\n", err)
					select {
					case streamErrChan <- fmt.Errorf("stream send error: %w", err):
					default:
					}
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

// ReceiveMessages returns a channel of incoming messages.
func (c *ClientImpl) ReceiveMessages(_ context.Context) <-chan Message {
	// Check connection status with read lock
	c.mu.RLock()
	connected := c.connected
	msgChan := c.msgChan
	c.mu.RUnlock()

	if !connected || msgChan == nil {
		// Return closed channel if not connected
		closedChan := make(chan Message)
		close(closedChan)
		return closedChan
	}

	// Return the transport's message channel directly
	return msgChan
}

// ReceiveResponse returns an iterator for the response messages.
func (c *ClientImpl) ReceiveResponse(_ context.Context) MessageIterator {
	// Check connection status with read lock
	c.mu.RLock()
	connected := c.connected
	transport := c.transport
	msgChan := c.msgChan
	errChan := c.errChan
	streamErrChan := c.streamErrChan
	c.mu.RUnlock()

	if !connected || msgChan == nil {
		closed := make(chan Message)
		close(closed)
		return &clientIterator{msgChan: closed, errChan: make(chan error)}
	}

	return &clientIterator{
		msgChan:       msgChan,
		errChan:       errChan,
		streamErrChan: streamErrChan,
		exitErr:       func() error { return exitFailure(transport) },
	}
}

// Interrupt stops the current turn by sending an interrupt control request to
// the CLI. The CLI process is not signalled: an interrupted turn still ends
// with its ResultMessage, and the client stays connected, ready for the next
// Query. Returns error if not connected or if the control request fails.
func (c *ClientImpl) Interrupt(ctx context.Context) error {
	transport, err := c.connectedTransport(ctx)
	if err != nil {
		return err
	}
	return transport.Interrupt(ctx)
}

// StopTask stops the running task identified by taskID, the TaskID of a
// TaskStartedMessage. The CLI reports the outcome with a
// TaskNotificationMessage whose Status is TaskNotificationStatusStopped.
// Returns error if not connected or if the control request fails.
//
// Example:
//
//	if started, ok := msg.(*claudecode.TaskStartedMessage); ok {
//	    err := client.StopTask(ctx, started.TaskID)
//	}
func (c *ClientImpl) StopTask(ctx context.Context, taskID string) error {
	transport, err := c.connectedTransport(ctx)
	if err != nil {
		return err
	}
	return transport.StopTask(ctx, taskID)
}

// BackgroundTasks moves in-flight foreground tasks (Bash commands and
// subagents) to the background: each blocking tool call returns at once and
// the turn continues, while the task keeps running and later reports a
// TaskNotificationMessage. A non-empty toolUseID targets only the task started
// by that tool_use block. It returns false only when toolUseID matched no
// foreground task.
// Returns error if not connected or if the control request fails.
func (c *ClientImpl) BackgroundTasks(ctx context.Context, toolUseID string) (bool, error) {
	transport, err := c.connectedTransport(ctx)
	if err != nil {
		return false, err
	}
	return transport.BackgroundTasks(ctx, toolUseID)
}

func (c *ClientImpl) connectedTransport(ctx context.Context) (Transport, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return c.liveTransport()
}

// SetModel changes the AI model during a streaming session.
// Pass nil to reset to the default model.
// Returns error if not connected or if the control request fails.
//
// Example - Change to a specific model:
//
//	model := "claude-sonnet-4-5"
//	err := client.SetModel(ctx, &model)
//
// Example - Reset to default model:
//
//	err := client.SetModel(ctx, nil)
func (c *ClientImpl) SetModel(ctx context.Context, model *string) error {
	// Check context before proceeding (Go idiom: fail fast)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	transport, err := c.liveTransport()
	if err != nil {
		return err
	}

	return transport.SetModel(ctx, model)
}

// SetPermissionMode changes the permission mode during a streaming session.
// Valid modes: PermissionModeDefault, PermissionModeAcceptEdits,
// PermissionModePlan, PermissionModeBypassPermissions.
// Returns error if not connected or if the control request fails.
//
// Example - Enable auto-accept for edits:
//
//	err := client.SetPermissionMode(ctx, claudecode.PermissionModeAcceptEdits)
//
// Example - Switch to plan mode:
//
//	err := client.SetPermissionMode(ctx, claudecode.PermissionModePlan)
func (c *ClientImpl) SetPermissionMode(ctx context.Context, mode PermissionMode) error {
	// Check context before proceeding (Go idiom: fail fast)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	transport, err := c.liveTransport()
	if err != nil {
		return err
	}

	return transport.SetPermissionMode(ctx, mode)
}

// RewindFiles reverts tracked files to their state at a specific user message.
// The messageUUID should be the UUID from a UserMessage received during the session.
// Requires file checkpointing to be enabled via WithFileCheckpointing() option.
// Returns error if not connected or the request fails.
//
// Example:
//
//	client := claudecode.NewClient(claudecode.WithFileCheckpointing())
//	// ... connect and receive messages, capture UUID from UserMessage
//	if msg, ok := receivedMsg.(*claudecode.UserMessage); ok && msg.UUID != nil {
//	    err := client.RewindFiles(ctx, *msg.UUID)
//	}
func (c *ClientImpl) RewindFiles(ctx context.Context, messageUUID string) error {
	// Check context before proceeding (Go idiom: fail fast)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	transport, err := c.liveTransport()
	if err != nil {
		return err
	}

	return transport.RewindFiles(ctx, messageUUID)
}

// GetMcpStatus returns the connection status of all configured MCP servers.
// Returns error if not connected or if the control request fails.
func (c *ClientImpl) GetMcpStatus(ctx context.Context) (*McpStatusResponse, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	transport, err := c.liveTransport()
	if err != nil {
		return nil, err
	}

	return transport.GetMcpStatus(ctx)
}

// clientIterator implements MessageIterator for client message reception
type clientIterator struct {
	msgChan       <-chan Message
	errChan       <-chan error
	streamErrChan <-chan error
	exitErr       func() error
	mu            sync.Mutex
	closed        bool
}

func (ci *clientIterator) Next(ctx context.Context) (Message, error) {
	ci.mu.Lock()
	if ci.closed {
		ci.mu.Unlock()
		return nil, ErrNoMoreMessages
	}
	ci.mu.Unlock()

	for {
		select {
		case msg, ok := <-ci.msgChan:
			if ok {
				return msg, nil
			}
			if ci.exitErr != nil {
				if err := ci.exitErr(); err != nil {
					return nil, ci.finish(err)
				}
			}
			return nil, ci.finish(ErrNoMoreMessages)
		case err, ok := <-ci.errChan:
			if !ok {
				// The error channel closes just before the message channel,
				// which may still hold buffered messages.
				ci.errChan = nil
				continue
			}
			return nil, ci.finish(err)
		case err := <-ci.streamErrChan:
			return nil, ci.finish(err)
		case <-ctx.Done():
			return nil, ci.finish(ctx.Err())
		}
	}
}

func (ci *clientIterator) finish(err error) error {
	ci.mu.Lock()
	ci.closed = true
	ci.mu.Unlock()
	return err
}

func (ci *clientIterator) Close() error {
	ci.mu.Lock()
	ci.closed = true
	ci.mu.Unlock()
	return nil
}

// GetStreamIssues returns validation issues found in the message stream.
// This can help diagnose problems like missing tool results or incomplete streams.
func (c *ClientImpl) GetStreamIssues() []StreamIssue {
	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()

	if transport == nil {
		return nil
	}

	validator := transport.GetValidator()
	if validator == nil {
		return nil
	}

	return validator.GetIssues()
}

// GetStreamStats returns statistics about the message stream.
// This includes counts of tools requested/received and pending tools.
func (c *ClientImpl) GetStreamStats() StreamStats {
	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()

	if transport == nil {
		return StreamStats{}
	}

	validator := transport.GetValidator()
	if validator == nil {
		return StreamStats{}
	}

	return validator.GetStats()
}

// GetServerInfo returns diagnostic information about the client and its connection.
// This provides useful information for debugging, health checks, and support scenarios.
//
// This method is thread-safe and can be called concurrently from multiple goroutines.
//
// Returns a map containing:
//   - "connected": bool - Whether the client is currently connected
//   - "transport_type": string - The type of transport being used (e.g., "subprocess")
//
// Returns an error if the client is not connected.
//
// Example:
//
//	info, err := client.GetServerInfo(ctx)
//	if err != nil {
//	    log.Printf("Client not connected: %v", err)
//	    return
//	}
//	fmt.Printf("Connected: %v, Transport: %s\n",
//	    info["connected"], info["transport_type"])
func (c *ClientImpl) GetServerInfo(_ context.Context) (map[string]interface{}, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.transport == nil {
		return nil, fmt.Errorf("client not connected")
	}

	info := map[string]interface{}{
		"connected":      true,
		"transport_type": "subprocess",
	}

	return info, nil
}

// Done returns a channel that is closed once the connected CLI process has exited.
func (c *ClientImpl) Done() <-chan struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.connected || c.transport == nil {
		return alreadyClosed
	}
	if w, ok := c.transport.(processWatcher); ok {
		return w.Done()
	}
	return c.disconnected
}

// Err returns nil while the connected CLI process is running, and why the
// client can no longer serve a turn once Done is closed.
func (c *ClientImpl) Err() error {
	_, err := c.liveTransport()
	return err
}

// liveTransport returns the transport if the client is connected and its CLI
// process is still running.
func (c *ClientImpl) liveTransport() (Transport, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.connected || c.transport == nil {
		return nil, errClientNotConnected
	}
	if w, ok := c.transport.(processWatcher); ok {
		if err := w.Err(); err != nil {
			return nil, err
		}
	}
	return c.transport, nil
}

// exitFailure returns the transport's exit error when its CLI process exited
// on its own with a non-zero status or a signal, and nil otherwise.
func exitFailure(transport Transport) error {
	w, ok := transport.(processWatcher)
	if !ok {
		return nil
	}
	err := w.Err()
	if procErr := AsProcessError(err); procErr != nil && procErr.ExitCode != 0 {
		return err
	}
	return nil
}
