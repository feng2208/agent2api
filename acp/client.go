package acp

import (
	"context"
	"encoding/json"
)

// AgentClient wraps RPCClient with typed ACP methods.
type AgentClient struct {
	rpc *RPCClient
}

func NewAgentClient(rpc *RPCClient) *AgentClient {
	return &AgentClient{rpc: rpc}
}

func (c *AgentClient) Initialize(ctx context.Context) (*InitializeResponse, error) {
	req := InitializeRequest{
		ProtocolVersion: 1,
		ClientCapabilities: ClientCapabilities{
			FS: FileSystemCapabilities{
				ReadTextFile:  false,
				WriteTextFile: false,
			},
			Terminal: false,
		},
		ClientInfo: Implementation{
			Name:    "acp-gateway",
			Title:   "ACP Gateway",
			Version: "0.1.0",
		},
	}
	var resp InitializeResponse
	if err := c.rpc.Call(ctx, "initialize", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *AgentClient) Authenticate(ctx context.Context, methodID string) error {
	req := AuthenticateRequest{MethodID: methodID}
	return c.rpc.Call(ctx, "authenticate", req, nil)
}

func (c *AgentClient) NewSession(ctx context.Context, cwd string) (*NewSessionResponse, error) {
	req := NewSessionRequest{
		Cwd:        cwd,
		McpServers: []McpServer{},
	}
	var resp NewSessionResponse
	if err := c.rpc.Call(ctx, "session/new", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *AgentClient) ResumeSession(ctx context.Context, sessionID, cwd string) error {
	req := ResumeSessionRequest{
		SessionID:  sessionID,
		Cwd:        cwd,
		McpServers: []McpServer{},
	}
	return c.rpc.Call(ctx, "session/resume", req, nil)
}

func (c *AgentClient) LoadSession(ctx context.Context, sessionID, cwd string) error {
	req := LoadSessionRequest{
		SessionID:  sessionID,
		Cwd:        cwd,
		McpServers: []McpServer{},
	}
	return c.rpc.Call(ctx, "session/load", req, nil)
}

func (c *AgentClient) ListSessions(ctx context.Context, cwd, cursor string) (*ListSessionsResponse, error) {
	req := ListSessionsRequest{
		Cwd:    cwd,
		Cursor: cursor,
	}
	var resp ListSessionsResponse
	if err := c.rpc.Call(ctx, "session/list", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *AgentClient) SetConfigOption(ctx context.Context, sessionID, option string, value any) error {
	req := SetConfigOptionRequest{
		SessionID: sessionID,
		Option:    option,
		Value:     value,
	}
	return c.rpc.Call(ctx, "session/set_config_option", req, nil)
}

func (c *AgentClient) SetMode(ctx context.Context, sessionID, mode string) error {
	req := SetModeRequest{
		SessionID: sessionID,
		Mode:      mode,
	}
	return c.rpc.Call(ctx, "session/set_mode", req, nil)
}

func (c *AgentClient) Prompt(ctx context.Context, sessionID string, prompt []PromptBlock) error {
	req := PromptRequest{
		SessionID: sessionID,
		Prompt:    prompt,
	}
	return c.rpc.Call(ctx, "session/prompt", req, nil)
}

func (c *AgentClient) CloseSession(ctx context.Context, sessionID string) error {
	req := CloseSessionRequest{SessionID: sessionID}
	return c.rpc.Notify("session/close", req)
}

func (c *AgentClient) CancelSession(ctx context.Context, sessionID string) error {
	req := CancelSessionRequest{SessionID: sessionID}
	return c.rpc.Notify("session/cancel", req)
}

// DefaultRequestHandler handles Auto-Approve for tool calls.
func DefaultRequestHandler(req *JSONRPCRequest) (json.RawMessage, *JSONRPCError) {
	// For fs/*, terminal/*, session/requestPermission we return approved
	// Note: The specific response structure might vary by method in standard ACP,
	// but generally returning an empty object or a specific outcome works.
	if req.Method == "session/requestPermission" {
		return json.RawMessage(`{"outcome": "approved"}`), nil
	}

	// Fallback empty result to signify success
	return json.RawMessage(`{}`), nil
}
