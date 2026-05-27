package acp

import "encoding/json"

// JSONRPCRequest represents a JSON-RPC 2.0 request or notification.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"` // nil for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *JSONRPCError) Error() string {
	if len(e.Data) > 0 {
		return e.Message + ": " + string(e.Data)
	}
	return e.Message
}

// --- ACP Specific Types ---

type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         Implementation     `json:"clientInfo"`
}

type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession,omitempty"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities,omitempty"`
}

type SessionCapabilities struct {
	Close           *struct{} `json:"close,omitempty"`
	List            *struct{} `json:"list,omitempty"`
	Resume          *struct{} `json:"resume,omitempty"`
	SetConfigOption *struct{} `json:"setConfigOption,omitempty"`
}

type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs"`
	Terminal bool                   `json:"terminal"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type AuthMethod struct {
	Type        string        `json:"type,omitempty"`
	ID          string        `json:"id"`
	Name        string        `json:"name,omitempty"`
	Description string        `json:"description,omitempty"`
	Vars        []AuthVarSpec `json:"vars,omitempty"`
}

type AuthVarSpec struct {
	Name string `json:"name"`
}

type AuthenticateRequest struct {
	MethodID string `json:"methodId"`
}

type NewSessionRequest struct {
	Cwd        string      `json:"cwd"`
	McpServers []McpServer `json:"mcpServers"`
}

type SelectOption struct {
	Value       any    `json:"value"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type ConfigOption struct {
	ID           string         `json:"id"`
	Name         string         `json:"name,omitempty"`
	Description  string         `json:"description,omitempty"`
	Category     string         `json:"category,omitempty"`
	Type         string         `json:"type,omitempty"`
	CurrentValue any            `json:"currentValue,omitempty"`
	Options      []SelectOption `json:"options,omitempty"`
}

type NewSessionResponse struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

type ResumeSessionRequest struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	McpServers []McpServer `json:"mcpServers"`
}

type LoadSessionRequest struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	McpServers []McpServer `json:"mcpServers"`
}

type ListSessionsRequest struct {
	Cwd    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type SessionInfo struct {
	SessionID string          `json:"sessionId"`
	Cwd       string          `json:"cwd"`
	Title     string          `json:"title,omitempty"`
	UpdatedAt string          `json:"updatedAt,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type McpServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
}

type SetConfigOptionRequest struct {
	SessionID string `json:"sessionId"`
	Option    string `json:"configId"`
	Value     any    `json:"value"`
}

type SetModeRequest struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"modeId"`
}

type PromptRequest struct {
	SessionID string        `json:"sessionId"`
	Prompt    []PromptBlock `json:"prompt"`
}

type PromptBlock struct {
	Type     string `json:"type"` // "text", "image", "embedded_terminal", etc.
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type CloseSessionRequest struct {
	SessionID string `json:"sessionId"`
}

type CancelSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// SessionUpdate represents a session/update notification.
type SessionUpdate struct {
	SessionID string     `json:"sessionId"`
	Update    TurnUpdate `json:"update"`
	Event     TurnUpdate `json:"event"`
}

// TurnUpdate handles ACP session/update payloads and keeps compatibility with
// older message_delta-style events used by early gateway code.
type TurnUpdate struct {
	SessionUpdate string          `json:"sessionUpdate,omitempty"`
	Type          string          `json:"type,omitempty"`
	Content       PromptBlock     `json:"content,omitempty"`
	Delta         json.RawMessage `json:"delta,omitempty"`
	Role          string          `json:"role,omitempty"`
}

type MessageDelta struct {
	Type string `json:"type"` // "text"
	Text string `json:"text,omitempty"`
}
