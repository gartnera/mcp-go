package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callModern sends a modern tools/call that declares the tasks extension.
func callModern(t *testing.T, srv *MCPServer, name string, args map[string]any) mcp.JSONRPCResponse {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args, "_meta": modernTaskMeta()})
	require.NoError(t, err)
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(params) + `}`
	response := srv.HandleMessage(t.Context(), []byte(req))
	resp, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok, "Expected JSONRPCResponse, got %T: %+v", response, response)
	return resp
}

// TestWithTaskDecision verifies that the server decides per request whether a
// TaskSupportOptional tool call becomes a task under the extension.
func TestWithTaskDecision(t *testing.T) {
	var decided []bool
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithTasksExtension(),
		WithTaskDecision(func(ctx context.Context, req mcp.CallToolRequest) bool {
			async := req.GetBool("async", false)
			decided = append(decided, async)
			return async
		}),
	)
	srv.AddTool(mcp.NewTool("work", mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("done"), nil
		})

	t.Run("declined runs synchronously", func(t *testing.T) {
		resp := callModern(t, srv, "work", map[string]any{"async": false})
		result, ok := resp.Result.(*mcp.CallToolResult)
		require.True(t, ok, "Expected *CallToolResult, got %T", resp.Result)
		require.Len(t, result.Content, 1)
		assert.Equal(t, "done", result.Content[0].(mcp.TextContent).Text)
	})

	t.Run("accepted runs as a task", func(t *testing.T) {
		resp := callModern(t, srv, "work", map[string]any{"async": true})
		result, ok := resp.Result.(*mcp.CreateTaskResult)
		require.True(t, ok, "Expected *CreateTaskResult, got %T", resp.Result)
		assert.Equal(t, mcp.ResultTypeTask, result.GetResultType())
	})

	assert.Equal(t, []bool{false, true}, decided)
}

// TestWithTaskDecision_RequiredUnaffected verifies that a TaskSupportRequired
// tool still runs as a task when the decision declines it.
func TestWithTaskDecision_RequiredUnaffected(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithTasksExtension(),
		WithTaskDecision(func(context.Context, mcp.CallToolRequest) bool { return false }),
	)
	srv.AddTool(mcp.NewTool("work", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("done"), nil
		})

	resp := callModern(t, srv, "work", nil)
	_, ok := resp.Result.(*mcp.CreateTaskResult)
	assert.True(t, ok, "Expected *CreateTaskResult, got %T", resp.Result)
}

type taskStartKey struct{}

// TestWithTaskStart verifies that the server's task start chooses the path,
// hands the handler its context either way, and sets the working task's
// status message and the create-task result's _meta.
func TestWithTaskStart(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithTasksExtension(),
		WithTaskStart(func(ctx context.Context, req mcp.CallToolRequest) TaskStart {
			async := req.GetBool("async", false)
			start := TaskStart{
				AsTask:  async,
				Context: context.WithValue(ctx, taskStartKey{}, "prepared"),
			}
			if async {
				start.StatusMessage = "working on it"
				start.Meta = &mcp.Meta{AdditionalFields: map[string]any{"note": "started"}}
			}
			return start
		}),
	)
	release := make(chan struct{})
	srv.AddTool(mcp.NewTool("work", mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			prepared, _ := ctx.Value(taskStartKey{}).(string)
			if req.GetBool("async", false) {
				<-release
			}
			return mcp.NewToolResultText(prepared), nil
		})

	t.Run("declined runs synchronously with the start's context", func(t *testing.T) {
		resp := callModern(t, srv, "work", map[string]any{"async": false})
		result, ok := resp.Result.(*mcp.CallToolResult)
		require.True(t, ok, "Expected *CallToolResult, got %T", resp.Result)
		require.Len(t, result.Content, 1)
		assert.Equal(t, "prepared", result.Content[0].(mcp.TextContent).Text)
	})

	t.Run("accepted starts a task with its status message and _meta", func(t *testing.T) {
		resp := callModern(t, srv, "work", map[string]any{"async": true})
		result, ok := resp.Result.(*mcp.CreateTaskResult)
		require.True(t, ok, "Expected *CreateTaskResult, got %T", resp.Result)
		assert.Equal(t, "working on it", result.Task.StatusMessage)
		require.NotNil(t, result.Meta)
		assert.Equal(t, "started", result.Meta.AdditionalFields["note"])

		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(encoded, &wire))
		assert.Equal(t, "working on it", wire["statusMessage"])
		meta, ok := wire["_meta"].(map[string]any)
		require.True(t, ok, "Expected a _meta object, got %T", wire["_meta"])
		assert.Equal(t, "started", meta["note"])

		task, _, err := srv.getTask(t.Context(), result.Task.TaskId)
		require.NoError(t, err)
		assert.Equal(t, "working on it", task.StatusMessage)

		close(release)
		entry, err := srv.getTaskEntry(t.Context(), result.Task.TaskId)
		require.NoError(t, err)
		<-entry.done
		srv.tasksMu.RLock()
		finished := entry.task
		stored := entry.result
		srv.tasksMu.RUnlock()
		assert.Equal(t, mcp.TaskStatusCompleted, finished.Status)
		assert.Empty(t, finished.StatusMessage, "the working status message outlived the task")
		callResult, ok := stored.(*mcp.CallToolResult)
		require.True(t, ok, "Expected *CallToolResult, got %T", stored)
		assert.Equal(t, "prepared", callResult.Content[0].(mcp.TextContent).Text)
	})
}

// TestWithTaskStart_AbortWhenTaskNotCreated verifies that what a task start
// began is undone when the task cannot be created.
func TestWithTaskStart_AbortWhenTaskNotCreated(t *testing.T) {
	aborted := 0
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithTasksExtension(),
		WithMaxConcurrentTasks(1),
		WithTaskStart(func(ctx context.Context, req mcp.CallToolRequest) TaskStart {
			return TaskStart{AsTask: true, Abort: func() { aborted++ }}
		}),
	)
	release := make(chan struct{})
	defer close(release)
	srv.AddTool(mcp.NewTool("work", mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			<-release
			return mcp.NewToolResultText("done"), nil
		})

	resp := callModern(t, srv, "work", nil)
	_, ok := resp.Result.(*mcp.CreateTaskResult)
	require.True(t, ok, "Expected *CreateTaskResult, got %T", resp.Result)
	assert.Equal(t, 0, aborted, "a created task was aborted")

	params, err := json.Marshal(map[string]any{"name": "work", "_meta": modernTaskMeta()})
	require.NoError(t, err)
	reply := srv.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+string(params)+`}`))
	_, isErr := reply.(mcp.JSONRPCError)
	require.True(t, isErr, "Expected a JSON-RPC error past the task limit, got %T: %+v", reply, reply)
	assert.Equal(t, 1, aborted)
}
