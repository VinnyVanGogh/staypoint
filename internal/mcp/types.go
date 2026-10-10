package mcp

import "encoding/json"

// JSON-RPC 2.0 request representation.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// JSON-RPC 2.0 response representation.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// JSON-RPC 2.0 error representation.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// MCP initialize result.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

// ServerCapabilities lists supported MCP features.
type ServerCapabilities struct {
	Tools map[string]any `json:"tools"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Property defines a JSON schema property.
type Property struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	// Items is the element schema of an "array" property.
	Items *Property `json:"items,omitempty"`
}

// InputSchema defines JSON Schema for tool input arguments.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

// Tool describes an available MCP tool.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

// ListToolsResult returns available tools.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams defines incoming parameters for tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolContent represents a single content element in tool results.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolCallResult defines the standard MCP tool call outcome.
type ToolCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError"`
}
