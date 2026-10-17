package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const promptPageSize = 1000

// addPrompts serves one prompt per profile that the caller can view. The
// middleware answers both prompt methods from the current profiles, so the
// SDK registry holds no prompt and no handler keeps profile contents.
func (s *server) addPrompts() {
	s.srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			switch method {
			case "prompts/list":
				return s.listPrompts(ctx, req.(*sdk.ListPromptsRequest))
			case "prompts/get":
				return s.readPrompt(ctx, req.(*sdk.GetPromptRequest))
			}
			return next(ctx, method, req)
		}
	})
}

func profilePrompt(p fw.AgentProfile) *sdk.Prompt {
	return &sdk.Prompt{
		Name: p.Name, Title: "Agent " + p.Name, Description: profileSummary(p),
		Arguments: []*sdk.PromptArgument{{
			Name: "input", Title: "Input",
			Description: "The task. It follows the system prompt of the profile as a user message.",
		}},
	}
}

// listPrompts filters before pagination, so neither entries nor cursors
// expose a profile that the caller cannot view. The cursor is the last name.
func (s *server) listPrompts(ctx context.Context, req *sdk.ListPromptsRequest) (*sdk.ListPromptsResult, error) {
	profiles, err := s.a.ListProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: list prompts: %w", err)
	}
	start := 0
	if req.Params != nil && req.Params.Cursor != "" {
		last, err := base64.RawURLEncoding.DecodeString(req.Params.Cursor)
		if err != nil || len(last) == 0 || len(last) > 64 {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid prompt cursor"}
		}
		start = sort.Search(len(profiles), func(i int) bool { return profiles[i].Name > string(last) })
	}
	end := min(start+promptPageSize, len(profiles))
	res := &sdk.ListPromptsResult{
		Cacheable: sdk.Cacheable{CacheScope: "private", TTLMs: 0},
		Prompts:   make([]*sdk.Prompt, 0, end-start),
	}
	for _, p := range profiles[start:end] {
		res.Prompts = append(res.Prompts, profilePrompt(p))
	}
	if end < len(profiles) {
		res.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(profiles[end-1].Name))
	}
	return res, nil
}

// readPrompt resolves the current profile and access rules on every read.
// A hidden profile has the same error as a missing one.
func (s *server) readPrompt(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
	profiles, err := s.a.ListProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: get prompt: %w", err)
	}
	for _, p := range profiles {
		if p.Name != req.Params.Name {
			continue
		}
		var msgs []*sdk.PromptMessage
		if strings.TrimSpace(p.System) != "" {
			msgs = append(msgs, &sdk.PromptMessage{Role: "user", Content: &sdk.TextContent{Text: p.System}})
		}
		if in := req.Params.Arguments["input"]; strings.TrimSpace(in) != "" {
			msgs = append(msgs, &sdk.PromptMessage{Role: "user", Content: &sdk.TextContent{Text: in}})
		}
		if len(msgs) == 0 {
			msgs = append(msgs, &sdk.PromptMessage{Role: "user", Content: &sdk.TextContent{
				Text: fmt.Sprintf("The %s profile has no system prompt. Start a run with the run tool and agent %q.", p.Name, p.Name),
			}})
		}
		return &sdk.GetPromptResult{Description: profileSummary(p), Messages: msgs}, nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown prompt %q", req.Params.Name)}
}
