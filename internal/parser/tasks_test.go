package parser

import (
	"encoding/json"
	"testing"

	"github.com/severity1/claude-agent-sdk-go/internal/shared"
)

func parseOneLine(t *testing.T, line string) shared.Message {
	t.Helper()
	messages, err := New().ProcessLine(line)
	if err != nil {
		t.Fatalf("ProcessLine: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	return messages[0]
}

func assertPtrEqual[T comparable](t *testing.T, field string, got *T, want T) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", field, want)
	} else if *got != want {
		t.Errorf("%s = %v, want %v", field, *got, want)
	}
}

func TestParseTaskStartedMessage(t *testing.T) {
	line := `{"type":"system","subtype":"task_started","task_id":"a1b2","owned_by_subagent":false,` +
		`"tool_use_id":"toolu_01","description":"Research the topic","subagent_type":"general-purpose",` +
		`"is_backgrounded":false,"spawn_depth":1,"task_type":"local_agent","prompt":"Go research",` +
		`"uuid":"u-1","session_id":"s-1"}`

	got := parseOneLine(t, line)
	msg, ok := got.(*shared.TaskStartedMessage)
	if !ok {
		t.Fatalf("got %T, want *shared.TaskStartedMessage", got)
	}
	if msg.Type() != shared.MessageTypeSystem || msg.Subtype != shared.SystemSubtypeTaskStarted {
		t.Errorf("type/subtype = %q/%q", msg.Type(), msg.Subtype)
	}
	if msg.TaskID != "a1b2" || msg.Description != "Research the topic" || msg.UUID != "u-1" || msg.SessionID != "s-1" {
		t.Errorf("unexpected scalar fields: %+v", msg)
	}
	assertPtrEqual(t, "ToolUseID", msg.ToolUseID, "toolu_01")
	assertPtrEqual(t, "SubagentType", msg.SubagentType, "general-purpose")
	assertPtrEqual(t, "IsBackgrounded", msg.IsBackgrounded, false)
	assertPtrEqual(t, "SpawnDepth", msg.SpawnDepth, 1)
	assertPtrEqual(t, "TaskType", msg.TaskType, "local_agent")
	if msg.Data["owned_by_subagent"] != false {
		t.Errorf("Data should keep unmodelled fields, got %v", msg.Data)
	}
}

func TestParseTaskProgressMessage(t *testing.T) {
	line := `{"type":"system","subtype":"task_progress","task_id":"a1b2","tool_use_id":"toolu_01",` +
		`"description":"Research the topic","subagent_type":"general-purpose",` +
		`"usage":{"total_tokens":1200,"tool_uses":3,"duration_ms":4500},"last_tool_name":"Read",` +
		`"uuid":"u-2","session_id":"s-1"}`

	got := parseOneLine(t, line)
	msg, ok := got.(*shared.TaskProgressMessage)
	if !ok {
		t.Fatalf("got %T, want *shared.TaskProgressMessage", got)
	}
	if msg.TaskID != "a1b2" {
		t.Errorf("TaskID = %q", msg.TaskID)
	}
	want := shared.TaskUsage{TotalTokens: 1200, ToolUses: 3, DurationMs: 4500}
	if msg.Usage != want {
		t.Errorf("Usage = %+v, want %+v", msg.Usage, want)
	}
	assertPtrEqual(t, "LastToolName", msg.LastToolName, "Read")
	if msg.Summary != nil {
		t.Errorf("Summary = %v, want nil", *msg.Summary)
	}
}

func TestParseTaskUpdatedMessage(t *testing.T) {
	line := `{"type":"system","subtype":"task_updated","task_id":"a1b2",` +
		`"patch":{"is_backgrounded":true,"status":"running"},"uuid":"u-3","session_id":"s-1"}`

	got := parseOneLine(t, line)
	msg, ok := got.(*shared.TaskUpdatedMessage)
	if !ok {
		t.Fatalf("got %T, want *shared.TaskUpdatedMessage", got)
	}
	assertPtrEqual(t, "Patch.IsBackgrounded", msg.Patch.IsBackgrounded, true)
	assertPtrEqual(t, "Patch.Status", msg.Patch.Status, "running")
	if msg.Patch.Error != nil || msg.Patch.EndTime != nil {
		t.Errorf("unchanged fields should be nil: %+v", msg.Patch)
	}
}

func TestParseTaskNotificationMessage(t *testing.T) {
	line := `{"type":"system","subtype":"task_notification","task_id":"a1b2","tool_use_id":"toolu_01",` +
		`"status":"stopped","output_file":"","summary":"Stopped by host",` +
		`"usage":{"total_tokens":10,"tool_uses":0,"duration_ms":7},"uuid":"u-4","session_id":"s-1"}`

	got := parseOneLine(t, line)
	msg, ok := got.(*shared.TaskNotificationMessage)
	if !ok {
		t.Fatalf("got %T, want *shared.TaskNotificationMessage", got)
	}
	if msg.Status != shared.TaskNotificationStatusStopped || msg.Summary != "Stopped by host" {
		t.Errorf("Status/Summary = %q/%q", msg.Status, msg.Summary)
	}
	if msg.Usage == nil || msg.Usage.DurationMs != 7 {
		t.Errorf("Usage = %+v", msg.Usage)
	}
}

func TestParseTaskMessageFallsBackToSystemMessage(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"missing_task_id", `{"type":"system","subtype":"task_started","description":"x"}`},
		{"task_id_wrong_type", `{"type":"system","subtype":"task_notification","task_id":7,"status":"completed"}`},
		{"usage_wrong_shape", `{"type":"system","subtype":"task_progress","task_id":"a","usage":"lots"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOneLine(t, tt.line)
			msg, ok := got.(*shared.SystemMessage)
			if !ok {
				t.Fatalf("got %T, want *shared.SystemMessage", got)
			}
			if msg.Data["subtype"] != msg.Subtype {
				t.Errorf("Data should carry the raw message, got %v", msg.Data)
			}
		})
	}
}

func TestTaskMessagesMarshalWithTypeAndSubtype(t *testing.T) {
	tests := []struct {
		msg     shared.Message
		subtype string
	}{
		{&shared.TaskStartedMessage{TaskID: "t"}, shared.SystemSubtypeTaskStarted},
		{&shared.TaskProgressMessage{TaskID: "t"}, shared.SystemSubtypeTaskProgress},
		{&shared.TaskUpdatedMessage{TaskID: "t"}, shared.SystemSubtypeTaskUpdated},
		{&shared.TaskNotificationMessage{TaskID: "t"}, shared.SystemSubtypeTaskNotification},
	}
	for _, tt := range tests {
		t.Run(tt.subtype, func(t *testing.T) {
			raw, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			reparsed := parseOneLine(t, string(raw))
			if reparsed.Type() != shared.MessageTypeSystem {
				t.Errorf("round-trip type = %q", reparsed.Type())
			}
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			if fields["type"] != "system" || fields["subtype"] != tt.subtype || fields["task_id"] != "t" {
				t.Errorf("marshalled %s", raw)
			}
			if _, isGeneric := reparsed.(*shared.SystemMessage); isGeneric {
				t.Errorf("round-trip of %s parsed as a generic SystemMessage", raw)
			}
		})
	}
}
