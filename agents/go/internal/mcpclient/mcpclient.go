// Package mcpclient consumes the Ops Copilot MCP server as a client toolset (Chapter 3.3).
//
// MCP is language-neutral: the same server (the Python `agent.mcp_server`) backs both tracks.
// mcptoolset launches it over stdio and adapts its tools into ADK tools the Go agent can call —
// no change to the agent beyond adding the toolset. This is the seam the gateway later slots
// into (Ch. 5.2): point the transport at the gateway instead of the raw server.
package mcpclient

import (
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

// OpsMCPToolset returns a toolset backed by the Ops Copilot MCP server, launched over stdio.
// Adjust the command / working directory to your checkout (the server lives in agents/python).
func OpsMCPToolset() (tool.Toolset, error) {
	command := exec.Command("uv", "run", "python", "-m", "agent.mcp_server") //nolint:noctx // long-lived MCP server process
	return mcptoolset.New(mcptoolset.Config{
		Transport: &mcp.CommandTransport{Command: command},
	})
}
