package mcp

import (
	"github.com/mark3labs/mcp-go/server"
)

const (
	ServerName    = "mindgraph"
	ServerVersion = "0.1.0"
)

// NewServer constructs an MCP server with the project's tool surface attached.
// Tools are registered in tools.go.
func NewServer(h *Handlers) *server.MCPServer {
	s := server.NewMCPServer(
		ServerName, ServerVersion,
		server.WithToolCapabilities(true),
	)
	h.Register(s)
	return s
}
