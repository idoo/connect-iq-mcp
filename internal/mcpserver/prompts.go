package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type promptDef struct {
	name, title, desc string
	args              []promptArg
	text              string // fmt template; %[N]s refers to args in order
}

// promptArg is one prompt argument; a non-empty def makes it optional.
type promptArg struct {
	name, desc, def string
}

// prompts are the server's prompt templates; toolset slices append theirs
// here when they add workflows worth a prompt.
var prompts = []promptDef{}

func (s *session) addPrompts() {
	for _, p := range prompts {
		args := make([]*mcp.PromptArgument, len(p.args))
		for i, a := range p.args {
			args[i] = &mcp.PromptArgument{Name: a.name, Description: a.desc, Required: a.def == ""}
		}
		s.srv.AddPrompt(&mcp.Prompt{Name: p.name, Title: p.title, Description: p.desc, Arguments: args}, p.handle)
	}
}

func (p promptDef) handle(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	vals := make([]any, len(p.args))
	for i, a := range p.args {
		v := strings.TrimSpace(req.Params.Arguments[a.name])
		if v == "" {
			v = a.def
		}
		if v == "" {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("prompt %s: missing required argument %q", p.name, a.name)}
		}
		vals[i] = v
	}
	return &mcp.GetPromptResult{
		Description: p.desc,
		Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: fmt.Sprintf(p.text, vals...)}}},
	}, nil
}
