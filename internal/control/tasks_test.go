package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// respondToFirstRequest waits for the protocol's first write, answers it with
// respond, and returns the decoded "request" object that was written.
func respondToFirstRequest(t *testing.T, transport *controlMockTransport, respond func(requestID string)) <-chan map[string]any {
	t.Helper()
	written := make(chan map[string]any, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			transport.mu.Lock()
			if len(transport.writtenData) == 0 {
				transport.mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				continue
			}
			raw := transport.writtenData[0]
			transport.mu.Unlock()

			var envelope struct {
				Type      string         `json:"type"`
				RequestID string         `json:"request_id"`
				Request   map[string]any `json:"request"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				close(written)
				return
			}
			written <- envelope.Request
			respond(envelope.RequestID)
			return
		}
		close(written)
	}()
	return written
}

func startTaskTestProtocol(t *testing.T) (context.Context, *controlMockTransport, *Protocol) {
	t.Helper()
	ctx, cancel := setupControlTestContext(t, 5*time.Second)
	t.Cleanup(cancel)

	transport := newControlMockTransport()
	protocol := NewProtocol(transport)
	assertControlNoError(t, protocol.Start(ctx))
	t.Cleanup(func() { _ = protocol.Close() })
	return ctx, transport, protocol
}

func TestStopTaskSendsRequest(t *testing.T) {
	ctx, transport, protocol := startTaskTestProtocol(t)

	written := respondToFirstRequest(t, transport, func(id string) {
		transport.injectResponse(id, nil)
	})

	if err := protocol.StopTask(ctx, "task-abc"); err != nil {
		t.Fatalf("StopTask: %v", err)
	}

	request := <-written
	if len(request) != 2 || request["subtype"] != "stop_task" || request["task_id"] != "task-abc" {
		t.Errorf("request = %v, want {subtype: stop_task, task_id: task-abc}", request)
	}
}

func TestStopTaskPropagatesCLIError(t *testing.T) {
	ctx, transport, protocol := startTaskTestProtocol(t)

	respondToFirstRequest(t, transport, func(id string) {
		transport.injectErrorResponse(id, "no such task")
	})

	err := protocol.StopTask(ctx, "task-missing")
	if err == nil {
		t.Fatal("expected error from CLI error response")
	}
	if !strings.Contains(err.Error(), "no such task") {
		t.Errorf("error = %v, want it to carry the CLI's message", err)
	}
}

func TestBackgroundTasks(t *testing.T) {
	tests := []struct {
		name        string
		toolUseID   string
		response    any
		wantRequest map[string]any
		want        bool
	}{
		{
			name:        "all_foreground_tasks_omits_tool_use_id",
			response:    map[string]any{"backgrounded": true},
			wantRequest: map[string]any{"subtype": "background_tasks"},
			want:        true,
		},
		{
			name:        "single_task_by_tool_use_id",
			toolUseID:   "toolu_1",
			response:    map[string]any{"backgrounded": true},
			wantRequest: map[string]any{"subtype": "background_tasks", "tool_use_id": "toolu_1"},
			want:        true,
		},
		{
			name:        "no_matching_task",
			toolUseID:   "toolu_nomatch",
			response:    map[string]any{"backgrounded": false},
			wantRequest: map[string]any{"subtype": "background_tasks", "tool_use_id": "toolu_nomatch"},
			want:        false,
		},
		{
			name:        "missing_field_counts_as_backgrounded",
			response:    nil,
			wantRequest: map[string]any{"subtype": "background_tasks"},
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, transport, protocol := startTaskTestProtocol(t)

			written := respondToFirstRequest(t, transport, func(id string) {
				transport.injectResponse(id, tt.response)
			})

			got, err := protocol.BackgroundTasks(ctx, tt.toolUseID)
			if err != nil {
				t.Fatalf("BackgroundTasks: %v", err)
			}
			if got != tt.want {
				t.Errorf("BackgroundTasks = %v, want %v", got, tt.want)
			}

			request := <-written
			if len(request) != len(tt.wantRequest) {
				t.Fatalf("request = %v, want %v", request, tt.wantRequest)
			}
			for k, v := range tt.wantRequest {
				if request[k] != v {
					t.Errorf("request[%q] = %v, want %v", k, request[k], v)
				}
			}
		})
	}
}
