package shared

import (
	"encoding/json"
)

// Message type constants
const (
	MessageTypeUser      = "user"
	MessageTypeAssistant = "assistant"
	MessageTypeSystem    = "system"
	MessageTypeResult    = "result"

	// Control protocol message types
	MessageTypeControlRequest  = "control_request"
	MessageTypeControlResponse = "control_response"

	// Partial message streaming type
	MessageTypeStreamEvent = "stream_event"

	// Session heartbeat carrying rate-limit window state. Emitted on
	// essentially every CLI session — even when nothing is constrained.
	MessageTypeRateLimitEvent = "rate_limit_event"
)

// Content block type constants
const (
	ContentBlockTypeText       = "text"
	ContentBlockTypeThinking   = "thinking"
	ContentBlockTypeToolUse    = "tool_use"
	ContentBlockTypeToolResult = "tool_result"
)

// AssistantMessageError represents error types in assistant messages.
type AssistantMessageError string

// AssistantMessageError constants for error type identification.
const (
	AssistantMessageErrorAuthFailed     AssistantMessageError = "authentication_failed"
	AssistantMessageErrorBilling        AssistantMessageError = "billing_error"
	AssistantMessageErrorRateLimit      AssistantMessageError = "rate_limit"
	AssistantMessageErrorInvalidRequest AssistantMessageError = "invalid_request"
	AssistantMessageErrorServer         AssistantMessageError = "server_error"
	AssistantMessageErrorUnknown        AssistantMessageError = "unknown"
)

// Message represents any message type in the Claude Code protocol.
type Message interface {
	Type() string
}

// ContentBlock represents any content block within a message.
type ContentBlock interface {
	BlockType() string
}

// UserMessage represents a message from the user.
type UserMessage struct {
	MessageType     string         `json:"type"`
	Content         interface{}    `json:"content"` // string or []ContentBlock
	UUID            *string        `json:"uuid,omitempty"`
	ParentToolUseID *string        `json:"parent_tool_use_id,omitempty"`
	ToolUseResult   map[string]any `json:"tool_use_result,omitempty"`
}

// Type returns the message type for UserMessage.
func (m *UserMessage) Type() string {
	return MessageTypeUser
}

// GetUUID returns the UUID or empty string if nil.
func (m *UserMessage) GetUUID() string {
	if m.UUID != nil {
		return *m.UUID
	}
	return ""
}

// GetParentToolUseID returns the parent tool use ID or empty string if nil.
func (m *UserMessage) GetParentToolUseID() string {
	if m.ParentToolUseID != nil {
		return *m.ParentToolUseID
	}
	return ""
}

// GetToolUseResult returns the tool use result metadata or nil if not present.
func (m *UserMessage) GetToolUseResult() map[string]any {
	return m.ToolUseResult
}

// HasToolUseResult returns true if tool use result metadata is present and non-empty.
func (m *UserMessage) HasToolUseResult() bool {
	return len(m.ToolUseResult) > 0
}

// MarshalJSON implements custom JSON marshaling for UserMessage
func (m *UserMessage) MarshalJSON() ([]byte, error) {
	type userMessage UserMessage
	temp := struct {
		Type string `json:"type"`
		*userMessage
	}{
		Type:        MessageTypeUser,
		userMessage: (*userMessage)(m),
	}
	return json.Marshal(temp)
}

// AssistantMessage represents a message from the assistant.
type AssistantMessage struct {
	MessageType     string                 `json:"type"`
	Content         []ContentBlock         `json:"content"`
	Model           string                 `json:"model"`
	Error           *AssistantMessageError `json:"error,omitempty"`
	ParentToolUseID *string                `json:"parent_tool_use_id,omitempty"`
}

// Type returns the message type for AssistantMessage.
func (m *AssistantMessage) Type() string {
	return MessageTypeAssistant
}

// GetParentToolUseID returns the parent tool use ID or empty string if nil.
// On assistant messages produced inside a subagent (Agent/Task tool), this
// identifies the orchestrator tool_use_id that spawned the subagent.
func (m *AssistantMessage) GetParentToolUseID() string {
	if m.ParentToolUseID != nil {
		return *m.ParentToolUseID
	}
	return ""
}

// HasError returns true if the message contains an error.
func (m *AssistantMessage) HasError() bool {
	return m.Error != nil
}

// GetError returns the error type or empty string if nil.
func (m *AssistantMessage) GetError() AssistantMessageError {
	if m.Error != nil {
		return *m.Error
	}
	return ""
}

// IsRateLimited returns true if the error is a rate limit error.
func (m *AssistantMessage) IsRateLimited() bool {
	return m.Error != nil && *m.Error == AssistantMessageErrorRateLimit
}

// MarshalJSON implements custom JSON marshaling for AssistantMessage
func (m *AssistantMessage) MarshalJSON() ([]byte, error) {
	type assistantMessage AssistantMessage
	temp := struct {
		Type string `json:"type"`
		*assistantMessage
	}{
		Type:             MessageTypeAssistant,
		assistantMessage: (*assistantMessage)(m),
	}
	return json.Marshal(temp)
}

// SystemMessage represents a system message.
type SystemMessage struct {
	MessageType string         `json:"type"`
	Subtype     string         `json:"subtype"`
	Data        map[string]any `json:"-"` // Preserve all original data
}

// Type returns the message type for SystemMessage.
func (m *SystemMessage) Type() string {
	return MessageTypeSystem
}

// MarshalJSON implements custom JSON marshaling for SystemMessage
func (m *SystemMessage) MarshalJSON() ([]byte, error) {
	data := make(map[string]any)
	for k, v := range m.Data {
		data[k] = v
	}
	data["type"] = MessageTypeSystem
	data["subtype"] = m.Subtype
	return json.Marshal(data)
}

// System message subtypes that carry a task's lifecycle. A task is a subagent
// spawned by the Agent/Task tool, a background Bash command, or similar work
// the CLI tracks by task ID.
const (
	SystemSubtypeTaskStarted      = "task_started"
	SystemSubtypeTaskProgress     = "task_progress"
	SystemSubtypeTaskUpdated      = "task_updated"
	SystemSubtypeTaskNotification = "task_notification"
)

// Terminal task statuses carried by TaskNotificationMessage.Status.
const (
	TaskNotificationStatusCompleted = "completed"
	TaskNotificationStatusFailed    = "failed"
	TaskNotificationStatusStopped   = "stopped"
)

// TaskUsage is the running cost of a task.
type TaskUsage struct {
	TotalTokens int   `json:"total_tokens"`
	ToolUses    int   `json:"tool_uses"`
	DurationMs  int64 `json:"duration_ms"`
}

// TaskStartedMessage announces a new task. TaskID is the handle StopTask takes;
// ToolUseID correlates the task with the tool_use block that started it.
type TaskStartedMessage struct {
	Subtype        string  `json:"subtype"`
	TaskID         string  `json:"task_id"`
	ToolUseID      *string `json:"tool_use_id,omitempty"`
	Description    string  `json:"description"`
	SubagentType   *string `json:"subagent_type,omitempty"`
	IsBackgrounded *bool   `json:"is_backgrounded,omitempty"`
	// SpawnDepth is 1 for a subagent spawned by the main agent and N+1 for one
	// spawned inside a depth-N subagent. It is unset for other kinds of task.
	SpawnDepth     *int    `json:"spawn_depth,omitempty"`
	TaskType       *string `json:"task_type,omitempty"`
	WorkflowName   *string `json:"workflow_name,omitempty"`
	Prompt         *string `json:"prompt,omitempty"`
	SkipTranscript *bool   `json:"skip_transcript,omitempty"`
	UUID           string  `json:"uuid,omitempty"`
	SessionID      string  `json:"session_id,omitempty"`
	// Data holds the message exactly as the CLI sent it, including fields this
	// type does not model.
	Data map[string]any `json:"-"`
}

// Type returns the message type for TaskStartedMessage.
func (m *TaskStartedMessage) Type() string {
	return MessageTypeSystem
}

// MarshalJSON implements custom JSON marshaling for TaskStartedMessage.
func (m *TaskStartedMessage) MarshalJSON() ([]byte, error) {
	type taskStartedMessage TaskStartedMessage
	return json.Marshal(struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		*taskStartedMessage
	}{
		Type:               MessageTypeSystem,
		Subtype:            SystemSubtypeTaskStarted,
		taskStartedMessage: (*taskStartedMessage)(m),
	})
}

// TaskProgressMessage reports a running task's progress.
type TaskProgressMessage struct {
	Subtype      string    `json:"subtype"`
	TaskID       string    `json:"task_id"`
	ToolUseID    *string   `json:"tool_use_id,omitempty"`
	Description  string    `json:"description"`
	SubagentType *string   `json:"subagent_type,omitempty"`
	Usage        TaskUsage `json:"usage"`
	LastToolName *string   `json:"last_tool_name,omitempty"`
	Summary      *string   `json:"summary,omitempty"`
	UUID         string    `json:"uuid,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	// Data holds the message exactly as the CLI sent it.
	Data map[string]any `json:"-"`
}

// Type returns the message type for TaskProgressMessage.
func (m *TaskProgressMessage) Type() string {
	return MessageTypeSystem
}

// MarshalJSON implements custom JSON marshaling for TaskProgressMessage.
func (m *TaskProgressMessage) MarshalJSON() ([]byte, error) {
	type taskProgressMessage TaskProgressMessage
	return json.Marshal(struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		*taskProgressMessage
	}{
		Type:                MessageTypeSystem,
		Subtype:             SystemSubtypeTaskProgress,
		taskProgressMessage: (*taskProgressMessage)(m),
	})
}

// TaskUpdatePatch holds the task fields that changed. Nil fields did not change.
type TaskUpdatePatch struct {
	// Status is one of pending, running, completed, failed, killed or paused.
	Status         *string `json:"status,omitempty"`
	Description    *string `json:"description,omitempty"`
	EndTime        *int64  `json:"end_time,omitempty"`
	TotalPausedMs  *int64  `json:"total_paused_ms,omitempty"`
	Error          *string `json:"error,omitempty"`
	IsBackgrounded *bool   `json:"is_backgrounded,omitempty"`
}

// TaskUpdatedMessage reports a change to a task's state, such as a foreground
// task moving to the background.
type TaskUpdatedMessage struct {
	Subtype   string          `json:"subtype"`
	TaskID    string          `json:"task_id"`
	Patch     TaskUpdatePatch `json:"patch"`
	UUID      string          `json:"uuid,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	// Data holds the message exactly as the CLI sent it.
	Data map[string]any `json:"-"`
}

// Type returns the message type for TaskUpdatedMessage.
func (m *TaskUpdatedMessage) Type() string {
	return MessageTypeSystem
}

// MarshalJSON implements custom JSON marshaling for TaskUpdatedMessage.
func (m *TaskUpdatedMessage) MarshalJSON() ([]byte, error) {
	type taskUpdatedMessage TaskUpdatedMessage
	return json.Marshal(struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		*taskUpdatedMessage
	}{
		Type:               MessageTypeSystem,
		Subtype:            SystemSubtypeTaskUpdated,
		taskUpdatedMessage: (*taskUpdatedMessage)(m),
	})
}

// TaskNotificationMessage reports that a task has finished. Status is one of
// the TaskNotificationStatus constants; a task ended by StopTask reports
// TaskNotificationStatusStopped.
type TaskNotificationMessage struct {
	Subtype        string     `json:"subtype"`
	TaskID         string     `json:"task_id"`
	ToolUseID      *string    `json:"tool_use_id,omitempty"`
	Status         string     `json:"status"`
	OutputFile     string     `json:"output_file"`
	Summary        string     `json:"summary"`
	Usage          *TaskUsage `json:"usage,omitempty"`
	SkipTranscript *bool      `json:"skip_transcript,omitempty"`
	UUID           string     `json:"uuid,omitempty"`
	SessionID      string     `json:"session_id,omitempty"`
	// Data holds the message exactly as the CLI sent it.
	Data map[string]any `json:"-"`
}

// Type returns the message type for TaskNotificationMessage.
func (m *TaskNotificationMessage) Type() string {
	return MessageTypeSystem
}

// MarshalJSON implements custom JSON marshaling for TaskNotificationMessage.
func (m *TaskNotificationMessage) MarshalJSON() ([]byte, error) {
	type taskNotificationMessage TaskNotificationMessage
	return json.Marshal(struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		*taskNotificationMessage
	}{
		Type:                    MessageTypeSystem,
		Subtype:                 SystemSubtypeTaskNotification,
		taskNotificationMessage: (*taskNotificationMessage)(m),
	})
}

// ResultMessage represents the final result of a conversation turn.
type ResultMessage struct {
	MessageType      string          `json:"type"`
	Subtype          string          `json:"subtype"`
	DurationMs       int             `json:"duration_ms"`
	DurationAPIMs    int             `json:"duration_api_ms"`
	IsError          bool            `json:"is_error"`
	Errors           []string        `json:"errors,omitempty"`
	NumTurns         int             `json:"num_turns"`
	SessionID        string          `json:"session_id"`
	TotalCostUSD     *float64        `json:"total_cost_usd,omitempty"`
	Usage            *map[string]any `json:"usage,omitempty"`
	Result           *string         `json:"result,omitempty"`
	StructuredOutput any             `json:"structured_output,omitempty"`
}

// Type returns the message type for ResultMessage.
func (m *ResultMessage) Type() string {
	return MessageTypeResult
}

// MarshalJSON implements custom JSON marshaling for ResultMessage
func (m *ResultMessage) MarshalJSON() ([]byte, error) {
	type resultMessage ResultMessage
	temp := struct {
		Type string `json:"type"`
		*resultMessage
	}{
		Type:          MessageTypeResult,
		resultMessage: (*resultMessage)(m),
	}
	return json.Marshal(temp)
}

// TextBlock represents text content.
type TextBlock struct {
	MessageType string `json:"type"`
	Text        string `json:"text"`
}

// BlockType returns the content block type for TextBlock.
func (b *TextBlock) BlockType() string {
	return ContentBlockTypeText
}

// ThinkingBlock represents thinking content with signature.
type ThinkingBlock struct {
	MessageType string `json:"type"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
}

// BlockType returns the content block type for ThinkingBlock.
func (b *ThinkingBlock) BlockType() string {
	return ContentBlockTypeThinking
}

// ToolUseBlock represents a tool use request.
type ToolUseBlock struct {
	MessageType string         `json:"type"`
	ToolUseID   string         `json:"tool_use_id"`
	Name        string         `json:"name"`
	Input       map[string]any `json:"input"`
}

// BlockType returns the content block type for ToolUseBlock.
func (b *ToolUseBlock) BlockType() string {
	return ContentBlockTypeToolUse
}

// ToolResultBlock represents the result of a tool use.
type ToolResultBlock struct {
	MessageType string      `json:"type"`
	ToolUseID   string      `json:"tool_use_id"`
	Content     interface{} `json:"content"` // string or structured data
	IsError     *bool       `json:"is_error,omitempty"`
}

// BlockType returns the content block type for ToolResultBlock.
func (b *ToolResultBlock) BlockType() string {
	return ContentBlockTypeToolResult
}

// RawControlMessage wraps raw control protocol messages for passthrough to the control handler.
// Control messages are not parsed into typed structs by the parser - they are routed directly
// to the control protocol handler which performs its own parsing.
type RawControlMessage struct {
	MessageType string
	Data        map[string]any
}

// Type returns the message type for RawControlMessage.
func (m *RawControlMessage) Type() string {
	return m.MessageType
}

// Rate-limit window status constants. Status carries one of these strings;
// "allowed" means the session is fine and the message is informational only.
const (
	RateLimitStatusAllowed = "allowed"
)

// RateLimitInfo carries the rate-limit window state from a rate_limit_event
// message. The Claude CLI emits one of these as a per-session heartbeat
// regardless of whether the user is actually constrained — check Status to
// decide if action is needed.
type RateLimitInfo struct {
	Status          string `json:"status"`
	ResetsAt        int64  `json:"resetsAt"`
	RateLimitType   string `json:"rateLimitType"`
	OverageStatus   string `json:"overageStatus,omitempty"`
	OverageResetsAt int64  `json:"overageResetsAt,omitempty"`
	IsUsingOverage  bool   `json:"isUsingOverage,omitempty"`
}

// RateLimitEventMessage is a session heartbeat from the CLI announcing the
// current rate-limit window. Emitted on essentially every session even when
// nothing is constrained — most consumers can simply ignore the message
// unless RateLimitInfo.Status differs from RateLimitStatusAllowed.
//
// See https://github.com/severity1/claude-agent-sdk-go/issues/126.
type RateLimitEventMessage struct {
	MessageType   string        `json:"type"`
	RateLimitInfo RateLimitInfo `json:"rate_limit_info"`
	UUID          string        `json:"uuid,omitempty"`
	SessionID     string        `json:"session_id,omitempty"`
}

// Type returns the message type for RateLimitEventMessage.
func (m *RateLimitEventMessage) Type() string {
	return MessageTypeRateLimitEvent
}

// IsAllowed returns true when the rate-limit window is healthy — i.e. the
// heartbeat is informational and the consumer can keep going.
func (m *RateLimitEventMessage) IsAllowed() bool {
	return m.RateLimitInfo.Status == RateLimitStatusAllowed
}

// Stream event type constants for Event["type"] discrimination.
// Use these when type-switching on StreamEvent.Event to handle different event types.
const (
	StreamEventTypeContentBlockStart = "content_block_start"
	StreamEventTypeContentBlockDelta = "content_block_delta"
	StreamEventTypeContentBlockStop  = "content_block_stop"
	StreamEventTypeMessageStart      = "message_start"
	StreamEventTypeMessageDelta      = "message_delta"
	StreamEventTypeMessageStop       = "message_stop"
)

// StreamEvent represents a partial message update during streaming.
// Emitted when IncludePartialMessages is enabled in Options.
//
// The Event field contains varying structure depending on event type:
//   - content_block_start: {"type": "content_block_start", "index": <int>, "content_block": {...}}
//   - content_block_delta: {"type": "content_block_delta", "index": <int>, "delta": {...}}
//   - content_block_stop: {"type": "content_block_stop", "index": <int>}
//   - message_start: {"type": "message_start", "message": {...}}
//   - message_delta: {"type": "message_delta", "delta": {...}, "usage": {...}}
//   - message_stop: {"type": "message_stop"}
//
// Consumer code should type-switch on Event["type"] to handle different event types:
//
//	switch event.Event["type"] {
//	case shared.StreamEventTypeContentBlockDelta:
//	    // Handle content delta
//	case shared.StreamEventTypeMessageStop:
//	    // Handle message completion
//	}
type StreamEvent struct {
	UUID            string         `json:"uuid"`
	SessionID       string         `json:"session_id"`
	Event           map[string]any `json:"event"`
	ParentToolUseID *string        `json:"parent_tool_use_id,omitempty"`
}

// Type returns the message type for StreamEvent.
func (m *StreamEvent) Type() string {
	return MessageTypeStreamEvent
}
