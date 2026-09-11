package mcpserver

import (
	"context"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// detachedToolContext is the agent.Context one MCP tool call runs under.
//
// A tool called over MCP has no invocation behind it: no session, no event
// stream, no conversation history, no human. Everything an agent.Context
// normally reaches is genuinely absent, and this type says so rather than
// inventing empty stand-ins — a nil session that silently reads as "no state"
// would turn a missing capability into a wrong answer.
//
// It is built by embedding the interface and overriding only what a read tool
// legitimately needs:
//
//   - the four standard context methods, so a deadline the transport set
//     actually reaches the database query;
//   - ToolConfirmation, which correctly reports that no approval has been
//     granted;
//   - RequestConfirmation, which refuses.
//
// Any other method reaches the embedded nil interface and panics. That is
// deliberate and it is loud, not silent: ADK's function-tool dispatcher
// recovers a panic from a tool and returns it as an error naming the tool, so a
// tool that needs session state fails visibly on its first MCP call instead of
// quietly returning something wrong. The alternative — implementing every
// method to return a zero value — is exactly the silent-downgrade failure this
// repository refuses everywhere else.
type detachedToolContext struct {
	agent.Context
	ctx context.Context //nolint:containedctx // this type *is* the context adapter; the field is its payload.
}

// The four standard context methods, served from the transport's context.

func (c detachedToolContext) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c detachedToolContext) Done() <-chan struct{}       { return c.ctx.Done() }
func (c detachedToolContext) Err() error                  { return c.ctx.Err() }
func (c detachedToolContext) Value(key any) any           { return c.ctx.Value(key) }

// ToolConfirmation reports that nothing has been approved.
//
// nil is the honest answer and the safe one: ADK reads it as "no approval is on
// file", which sends a confirmation-requiring tool to RequestConfirmation
// below. Returning a confirmed value here would forge an approval no human ever
// gave.
func (c detachedToolContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// RequestConfirmation refuses, because there is nobody to ask.
//
// This is the enforcement that does not depend on configuration: whatever tools
// were registered, one that pauses for human approval cannot complete over MCP.
// See [ErrConfirmationUnavailable].
func (c detachedToolContext) RequestConfirmation(string, any) error {
	return ErrConfirmationUnavailable
}
