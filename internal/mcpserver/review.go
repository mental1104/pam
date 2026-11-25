package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/mental1104/pam/internal/app"
	"github.com/mental1104/pam/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// reviewArguments 是受限业务操作参数；Request/Decision 分别用于预检及人工决定，不允许 SQL 写入。
type reviewArguments struct {
	Action   string                `json:"action"`
	ID       string                `json:"id,omitempty"`
	State    string                `json:"state,omitempty"`
	Limit    int                   `json:"limit,omitempty"`
	Offset   int                   `json:"offset,omitempty"`
	Request  domain.ImportRequest  `json:"request,omitempty"`
	Decision domain.ReviewDecision `json:"decision,omitempty"`
}

// registerReviewTools 将 server 接到 application 的同一导入逻辑；工具调用仍必须经过外层认证。
func registerReviewTools(server *mcp.Server, application *app.Service) {
	object := map[string]any{"type": "object"}
	properties := map[string]any{"action": map[string]any{"type": "string", "enum": []string{"preview", "get", "confirm", "reviews", "resolve", "ranges"}}, "id": map[string]any{"type": "string"}, "state": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}, "offset": map[string]any{"type": "integer"}, "request": importSchema(), "decision": object}
	server.AddTool(&mcp.Tool{Name: "manage_imports", Description: "Preview normalized transactions without ledger writes; get a proposal by id; confirm explicitly to import undisputed rows; list persistent reviews; resolve with decision action link/create/correct/dismiss and reason (record required for create/correct); ranges reports observed records, never complete statement coverage. Resolve and confirm require explicit human intent.", InputSchema: map[string]any{"type": "object", "properties": properties, "required": []string{"action"}, "additionalProperties": false}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input reviewArguments
		if err := decode(r.Params.Arguments, &input); err != nil {
			return toolError(domain.ValidationError{Message: "invalid import management request"}), nil
		}
		var result any
		var err error
		switch input.Action {
		case "preview":
			result, err = application.Propose(ctx, input.Request)
		case "get":
			result, err = application.Proposal(ctx, input.ID)
		case "confirm":
			result, err = application.Confirm(ctx, input.ID)
		case "reviews":
			if input.Limit == 0 {
				input.Limit = 50
			}
			result, err = application.Reviews(ctx, input.State, input.Limit, input.Offset)
		case "resolve":
			var decision json.RawMessage
			decision, err = application.Resolve(ctx, input.ID, input.Decision)
			result = decision
		case "ranges":
			result, err = application.SourceRanges(ctx)
		default:
			err = domain.ValidationError{Message: "unsupported import management action"}
		}
		if err != nil {
			return toolError(err), nil
		}
		return toolResult(result)
	})
}
