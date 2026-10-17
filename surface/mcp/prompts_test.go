package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func promptNames(t *testing.T, ctx context.Context, cs *sdk.ClientSession) []string {
	t.Helper()
	var names []string
	for p, err := range cs.Prompts(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.Name)
	}
	return names
}

func unknownPrompt(t *testing.T, ctx context.Context, cs *sdk.ClientSession, name string) {
	t.Helper()
	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: name})
	var rpcErr *jsonrpc.Error
	if res != nil || !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams || rpcErr.Message != fmt.Sprintf("unknown prompt %q", name) {
		t.Fatalf("get hidden or missing prompt %q = %+v, %v", name, res, err)
	}
}

func promptText(t *testing.T, ctx context.Context, cs *sdk.ClientSession, name string) string {
	t.Helper()
	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("prompt %q = %+v", name, res)
	}
	text, ok := res.Messages[0].Content.(*sdk.TextContent)
	if !ok {
		t.Fatalf("prompt %q content = %T", name, res.Messages[0].Content)
	}
	return text.Text
}

func TestProfileDiscoveryDoesNotLeak(t *testing.T) {
	for _, protocol := range []string{modernProtocol, "2025-11-25"} {
		t.Run(protocol, func(t *testing.T) {
			ctx := testCtx(t)
			const hiddenName, hiddenText = "hidden-profile-sentinel", "Private instructions sentinel."
			a := manageAgent(t, newScript().model(), fw.WithProfile(fw.AgentProfile{
				Name: hiddenName, System: hiddenText, Access: &fw.Access{Owners: []string{"carol"}},
			}))
			cs := mustConnect(t, ctx, serveAgent(t, a, nil).URL, clientSetup{
				protocol: protocol, header: map[string][]string{"Authorization": {"Bearer bob-token"}},
			})
			if got := cs.InitializeResult().Instructions; strings.Contains(got, hiddenName) || strings.Contains(got, hiddenText) {
				t.Errorf("discovery exposed a private profile: %s", got)
			}
			tools, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(tools)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), hiddenName) || strings.Contains(string(b), hiddenText) {
				t.Error("tools/list exposed a private profile")
			}
			if names := promptNames(t, ctx, cs); !slices.Equal(names, []string{"reviewer"}) {
				t.Errorf("bob's prompts = %v", names)
			}
			unknownPrompt(t, ctx, cs, hiddenName)
			unknownPrompt(t, ctx, cs, "missing-profile")
		})
	}
}

func TestPromptsFollowProfileChanges(t *testing.T) {
	for _, protocol := range []string{modernProtocol, "2025-11-25"} {
		t.Run(protocol, func(t *testing.T) { testPromptsFollowProfileChanges(t, protocol) })
	}
}

func testPromptsFollowProfileChanges(t *testing.T, protocol string) {
	t.Helper()
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	url := serveAgent(t, a, nil).URL
	alice := mustConnect(t, ctx, url, clientSetup{protocol: protocol, header: http.Header{"Authorization": {"Bearer alice-token"}}})
	bob := mustConnect(t, ctx, url, clientSetup{protocol: protocol, header: http.Header{"Authorization": {"Bearer bob-token"}}})
	put := func(system string) {
		t.Helper()
		res := callTool(t, ctx, alice, toolAgentPut, map[string]any{
			"name": "fresh", "system": system,
			"access": map[string]any{"owners": []string{"alice"}, "viewers": []string{"bob"}},
		})
		if res.IsError {
			t.Fatal(textOf(res))
		}
	}
	put("First prompt.")
	if names := promptNames(t, ctx, bob); !slices.Equal(names, []string{"fresh", "reviewer"}) {
		t.Fatalf("prompts after create = %v", names)
	}
	if got := promptText(t, ctx, bob, "fresh"); got != "First prompt." {
		t.Fatalf("created prompt = %q", got)
	}
	put("Updated prompt.")
	if got := promptText(t, ctx, bob, "fresh"); got != "Updated prompt." {
		t.Fatalf("updated prompt = %q", got)
	}
	// Changes outside the MCP server also take effect on the next request.
	next := a.ProfileSet()
	next["fresh"] = fw.AgentProfile{Name: "fresh", System: "Private now.", Access: &fw.Access{Owners: []string{"alice"}}}
	if err := a.SetProfiles(next); err != nil {
		t.Fatal(err)
	}
	unknownPrompt(t, ctx, bob, "fresh")
	if names := promptNames(t, ctx, bob); !slices.Equal(names, []string{"reviewer"}) {
		t.Fatalf("prompts after revocation = %v", names)
	}
	if got := promptText(t, ctx, alice, "fresh"); got != "Private now." {
		t.Fatalf("owner's prompt = %q", got)
	}
	res := callTool(t, ctx, alice, toolAgentDelete, map[string]any{"name": "fresh"})
	if res.IsError {
		t.Fatal(textOf(res))
	}
	unknownPrompt(t, ctx, alice, "fresh")
	if slices.Contains(promptNames(t, ctx, alice), "fresh") {
		t.Fatal("deleted profile remains in prompts/list")
	}
	put("Recreated prompt.")
	if got := promptText(t, ctx, bob, "fresh"); got != "Recreated prompt." {
		t.Fatalf("recreated prompt = %q", got)
	}
	list, err := bob.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list.CacheScope != "private" || list.TTLMs != 0 {
		t.Errorf("prompt cache = %+v", list.Cacheable)
	}
	if caps := bob.InitializeResult().Capabilities; caps.Prompts == nil || caps.Prompts.ListChanged {
		t.Errorf("prompt capabilities = %+v", caps.Prompts)
	}
}

func TestPromptPaginationFiltersBeforePaging(t *testing.T) {
	ctx := testCtx(t)
	cfg := testConfig(t)
	cfg.Access.Default = "closed"
	var want []string
	for i := range promptPageSize + 1 {
		name := fmt.Sprintf("visible-%04d", i)
		cfg.Agents[name] = fw.AgentProfile{Name: name, Access: &fw.Access{Viewers: []string{"bob"}}}
		want = append(want, name)
	}
	for _, name := range []string{"a-hidden", "z-hidden"} {
		cfg.Agents[name] = fw.AgentProfile{Name: name, Access: &fw.Access{Owners: []string{"carol"}}}
	}
	a := newAgent(t, newScript().model(), fw.WithConfig(cfg), fw.WithAuthn(people))
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "bob-token")
	first, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Prompts) != promptPageSize || first.NextCursor == "" {
		t.Fatalf("first page has %d prompts, cursor %q", len(first.Prompts), first.NextCursor)
	}
	last, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil || string(last) != want[promptPageSize-1] {
		t.Fatalf("cursor = %q, %v", last, err)
	}
	second, err := cs.ListPrompts(ctx, &sdk.ListPromptsParams{Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Prompts) != 1 || second.Prompts[0].Name != want[promptPageSize] || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	if names := promptNames(t, ctx, cs); !slices.Equal(names, want) {
		t.Fatalf("paged list has %d names, want %d", len(names), len(want))
	}
	for _, cursor := range []string{"%%%", base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 65)))} {
		res, err := cs.ListPrompts(ctx, &sdk.ListPromptsParams{Cursor: cursor})
		var rpcErr *jsonrpc.Error
		if res != nil || !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("invalid cursor = %+v, %v", res, err)
		}
	}
}

func TestPromptsConcurrentCallers(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model(), fw.WithProfile(fw.AgentProfile{
		Name: "alice-only", System: "Only Alice can read this.", Access: &fw.Access{Owners: []string{"alice"}},
	}))
	url := serveAgent(t, a, nil).URL
	alice, bob := as(t, ctx, url, "alice-token"), as(t, ctx, url, "bob-token")
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs, owner := alice, i%2 == 0
			if !owner {
				cs = bob
			}
			for range 10 {
				list, err := cs.ListPrompts(ctx, nil)
				if err != nil {
					errs <- err
					return
				}
				found := slices.ContainsFunc(list.Prompts, func(p *sdk.Prompt) bool { return p.Name == "alice-only" })
				if found != owner {
					errs <- fmt.Errorf("owner=%v: private prompt listed=%v", owner, found)
					return
				}
				res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "alice-only"})
				if owner && (err != nil || res == nil) || !owner && (err == nil || res != nil) {
					errs <- fmt.Errorf("owner=%v: prompts/get = %+v, %v", owner, res, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPromptsUseStdioPrincipal(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model())
	st, ct := sdk.NewInMemoryTransports()
	sctx, cancel := context.WithCancel(fw.WithPrincipal(ctx, people["bob-token"]))
	defer cancel()
	go func() { _ = serve(sctx, a, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "prompt-client", Version: "v1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if names := promptNames(t, ctx, cs); !slices.Equal(names, []string{"reviewer"}) {
		t.Fatalf("stdio prompts = %v", names)
	}
	unknownPrompt(t, ctx, cs, "private")
}

func TestPromptsFailClosedWhenAuthzUnavailable(t *testing.T) {
	ctx := testCtx(t)
	a := manageAgent(t, newScript().model(), fw.WithAuthz(fw.AuthzFunc(func(context.Context, *fw.Principal, string, fw.Target) error {
		return fmt.Errorf("%w: test decision point", fw.ErrAuthzUnavailable)
	})))
	cs := as(t, ctx, serveAgent(t, a, nil).URL, "alice-token")
	list, err := cs.ListPrompts(ctx, nil)
	if list != nil || err == nil || !strings.Contains(err.Error(), fw.ErrAuthzUnavailable.Error()) {
		t.Fatalf("prompts/list with authz down = %+v, %v", list, err)
	}
	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "reviewer"})
	if res != nil || err == nil || !strings.Contains(err.Error(), fw.ErrAuthzUnavailable.Error()) {
		t.Fatalf("prompts/get with authz down = %+v, %v", res, err)
	}
}
