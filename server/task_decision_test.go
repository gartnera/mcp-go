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
