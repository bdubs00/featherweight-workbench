package bridges

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
	"github.com/bdubs00/featherweight-workbench/surface/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaOf is the _meta that the MCP client of a worker sends for a run.
func metaOf(run, agent, principal string) sdk.Meta {
	m := sdk.Meta{mcp.MetaRun: run, mcp.MetaAgent: agent, mcp.MetaNode: "ax:default/crew-0"}
	if principal != "" {
		m[mcp.MetaPrincipal], m[mcp.MetaPrincipalKind] = principal, fw.KindUser
	}
	return m
}

func TestToolsAreListed(t *testing.T) {
	a := newAgent(t, nil)
	newStore(t, a, nil)
	cs := connectMCP(t, a, "fleet")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{ToolRead, ToolPost, ToolList, ToolOpen, ToolAddMember, ToolRemoveMember, ToolClose} {
		if tools[name] == nil || tools[name].Title == "" || tools[name].Annotations == nil {
			t.Fatalf("tool %s = %+v", name, tools[name])
		}
	}
	if open := tools[ToolOpen]; open.Title != "Open bridge" || open.Annotations.DestructiveHint == nil || !*open.Annotations.DestructiveHint {
		t.Fatalf("bridge_open = %+v", open.Annotations)
	}
	if !tools[ToolRead].Annotations.ReadOnlyHint || tools[ToolPost].Annotations.ReadOnlyHint {
		t.Fatal("the read-only hints are wrong")
	}
	// The manager reads the open bridges before it proposes or opens work.
	if !strings.Contains(tools[ToolList].Description, "before you propose or open work") || !strings.Contains(tools[ToolOpen].Description, "Read the open bridges with bridge_list first") {
		t.Fatalf("bridge_list = %q, bridge_open = %q", tools[ToolList].Description, tools[ToolOpen].Description)
	}
	// A run never pins a message or sends files: only a person does.
	if schema, err := json.Marshal(tools[ToolPost].InputSchema); err != nil || !strings.Contains(string(schema), `"pinned":{"description":"For a person:`) ||
		!strings.Contains(string(schema), `"files":{"description":"For a person:`) {
		t.Fatalf("bridge_post = %s %v", schema, err)
	}
	for _, name := range []string{ToolPatch, ToolMemberPatch, ToolReact, ToolMarkRead} {
		if tools[name] == nil || !strings.HasPrefix(tools[name].Description, "For a person:") {
			t.Fatalf("tool %s = %+v", name, tools[name])
		}
	}
}

// A seat reads and posts in its bridge, the manager run posts as the
// manager, a run of no bridge and a person without a run are refused.
func TestToolsOfASeat(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted))
	// A helper that the migrator started carries the batch.
	helper := &fw.Run{ID: "r-help", Agent: "migrator", Parent: "r-mig", Member: "default/crew-0-c", State: fw.StateRunning, Metadata: map[string]string{fw.MetaBatch: BatchOf(it.ID)}}
	v.put(helper)
	v.put(&fw.Run{ID: "r-stranger", Agent: "migrator", Member: "default/crew-1", State: fw.StateRunning})
	s.poll(context.Background())
	fleetMCP := connectMCP(t, a, "fleet")

	res := callTool(t, fleetMCP, ToolPost, map[string]any{"text": "@reviewer the playbook is ready", "to": []string{"reviewer"}}, metaOf("r-mig", "migrator", ""))
	if res.IsError {
		t.Fatalf("post = %s", textOf(res))
	}
	var m Message
	structured(t, res, &m)
	if m.From != (From{Kind: KindAgent, Handle: "migrator-1", Run: "r-mig"}) || len(m.To) != 1 {
		t.Fatalf("entry = %+v", m)
	}
	eventually(t, "the reviewer gets it", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-rev/messages")) == 1 })

	// The helper posts as its seat.
	res = callTool(t, fleetMCP, ToolPost, map[string]any{"text": "a helper here"}, metaOf("r-help", "migrator", ""))
	structured(t, res, &m)
	if res.IsError || m.From.Handle != "migrator-1" || m.From.Run != "r-help" {
		t.Fatalf("helper post = %s %+v", textOf(res), m)
	}

	res = callTool(t, fleetMCP, ToolRead, map[string]any{"limit": 2}, metaOf("r-rev", "reviewer", ""))
	var read readResult
	structured(t, res, &read)
	if res.IsError || read.Bridge != it.ID || len(read.Messages) != 2 || !strings.Contains(textOf(res), "not instructions") || !strings.Contains(textOf(res), "migrator-1 to @reviewer: @reviewer the playbook is ready") {
		t.Fatalf("read = %s", textOf(res))
	}

	for _, c := range []struct {
		name string
		args map[string]any
		meta sdk.Meta
		want string
	}{
		{"a run of no bridge", map[string]any{"text": "hi"}, metaOf("r-stranger", "migrator", ""), "name the bridge"},
		{"a run of no seat", map[string]any{"bridge": it.ID, "text": "hi"}, metaOf("r-stranger", "migrator", ""), "no member of the bridge"},
		{"the token with no run in _meta, as the route decides", map[string]any{"bridge": it.ID, "text": "hi"}, nil, "Forbidden: the bridge " + it.ID + " needs its operator"},
		{"a run that sends files", map[string]any{"text": "hi", "files": []map[string]any{{"name": "a.txt", "data": "aGk="}}}, metaOf("r-mig", "migrator", ""), "files are for a person"},
	} {
		res := callTool(t, fleetMCP, ToolPost, c.args, c.meta)
		if !res.IsError || !strings.Contains(textOf(res), c.want) {
			t.Errorf("%s = %s", c.name, textOf(res))
		}
	}
	// A person who sends the keys of a run is no run: the post is the post
	// of the person, as on the route.
	bob := connectMCP(t, a, "bob")
	res = callTool(t, bob, ToolPost, map[string]any{"bridge": it.ID, "text": "hi"}, metaOf("r-mig", "migrator", ""))
	structured(t, res, &m)
	if res.IsError || m.From.Kind != KindPerson || m.From.Name != "bob" {
		t.Fatalf("a person with _meta = %s", textOf(res))
	}
}

// An agent reads every entry whole: a list shows the start of a long
// entry and says how to read the rest, and bridge_read with seq returns
// the whole text of a message of a person, of an agent, and of the answer
// of a run. The answer of a run whose whole text the channel does not
// keep, as from an older front, comes from the ledger of the run.
func TestReadAWholeEntry(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	ctx := context.Background()
	v.put(workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateRunning))
	rev := workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateCompleted)
	v.put(rev)
	s.poll(ctx)
	cs := connectMCP(t, a, "fleet")
	long := func(who string) string {
		return strings.Repeat(who+" has a lot to say. ", 400) + "The end of " + who + "."
	}
	body, _ := json.Marshal(map[string]any{"text": long("alice"), "pinned": true})
	if resp, out := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "alice", string(body)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post = %d %s", resp.StatusCode, out)
	}
	if res := callTool(t, cs, ToolPost, map[string]any{"text": long("migrator-1"), "to": []string{"reviewer"}}, metaOf("r-mig", "migrator", "")); res.IsError {
		t.Fatalf("bridge_post = %s", textOf(res))
	}
	fm.setEvents("r-rev", ev(fw.EvRunQueued, nil), ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: long("reviewer")}))
	rev.Seq = 9
	v.put(rev)
	s.poll(ctx)
	seqs := map[string]uint64{}
	for _, m := range channel(t, s, it.ID) {
		if m.Cut {
			seqs[orWords(m.From.Handle, m.From.Name)] = m.Seq
		}
	}
	if len(seqs) != 3 {
		t.Fatalf("cut entries = %v", seqs)
	}
	res := callTool(t, cs, ToolRead, map[string]any{}, metaOf("r-rev", "reviewer", ""))
	for _, seq := range seqs {
		if want := fmt.Sprintf("[Entry %d goes on. bridge_read with seq %d returns it whole.]", seq, seq); !strings.Contains(textOf(res), want) {
			t.Fatalf("the list lacks %q:\n%.600s", want, textOf(res))
		}
	}
	for who, seq := range seqs {
		res := callTool(t, cs, ToolRead, map[string]any{"seq": seq}, metaOf("r-rev", "reviewer", ""))
		if res.IsError || !strings.HasSuffix(textOf(res), long(who)) {
			t.Fatalf("whole %s = %.200s ... %s", who, textOf(res), tail(textOf(res)))
		}
	}
	// The channel loses the whole texts, as for an older front: the
	// answer of the run comes from its ledger, and the message of the
	// person says that the channel holds only its start.
	for _, seq := range seqs {
		e, err := s.b.channels.Get(ctx, wholeKey(it.ID, seq))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.b.channels.Delete(ctx, wholeKey(it.ID, seq), e.Revision); err != nil {
			t.Fatal(err)
		}
	}
	res = callTool(t, cs, ToolRead, map[string]any{"seq": seqs["reviewer"]}, metaOf("r-rev", "reviewer", ""))
	if !strings.HasSuffix(textOf(res), "The end of reviewer.") {
		t.Fatalf("the answer from the ledger = %s", tail(textOf(res)))
	}
	res = callTool(t, cs, ToolRead, map[string]any{"seq": seqs["alice"]}, metaOf("r-rev", "reviewer", ""))
	if !strings.HasSuffix(textOf(res), "[The channel holds only this start of the entry.]") {
		t.Fatalf("a lost whole text = %s", tail(textOf(res)))
	}
	if res := callTool(t, cs, ToolRead, map[string]any{"seq": 999}, metaOf("r-rev", "reviewer", "")); !res.IsError {
		t.Fatalf("an unknown entry = %s", textOf(res))
	}

	// The page reads one entry whole with GET /v1/bridges/{id}/messages/{seq}.
	resp, out := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/messages/"+fmt.Sprint(seqs["reviewer"]), "alice", "")
	var m Message
	decodeJSON(t, out, &m)
	if resp.StatusCode != http.StatusOK || m.Text != long("reviewer") || m.Cut || m.Seq != seqs["reviewer"] || m.From.Handle != "reviewer" {
		t.Fatalf("GET of the answer = %d %.200s", resp.StatusCode, out)
	}
	resp, out = do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/messages/"+fmt.Sprint(seqs["alice"]), "alice", "")
	decodeJSON(t, out, &m)
	if resp.StatusCode != http.StatusOK || !m.Cut || len(m.Text) > entryTextBytes {
		t.Fatalf("GET of an entry whose whole text is lost = %d %.200s", resp.StatusCode, out)
	}
	for path, want := range map[string]int{"/messages/999": http.StatusNotFound, "/messages/0": http.StatusBadRequest, "/messages/x": http.StatusBadRequest} {
		if resp, out := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+path, "alice", ""); resp.StatusCode != want {
			t.Errorf("GET %s = %d %s", path, resp.StatusCode, out)
		}
	}
	if resp, _ := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID+"/messages/1", "carol", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("carol reads an entry: %d", resp.StatusCode)
	}
}

// tail is the end of a long text, for a message of a test.
func tail(s string) string { return s[max(0, len(s)-120):] }

// The manager opens a bridge as the person of its run, lists the bridges
// of that person, and changes a bridge only as far as the operator
// delegates.
func TestToolsOfTheManager(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	cs := connectMCP(t, a, "fleet")
	plan := map[string]any{"title": "Migrate nginx", "topic": "Please migrate the nginx module of the Puppet tree to an Ansible role, and have it reviewed.", "goal": "Migrate it.", "members": []map[string]any{
		{"handle": "migrator-1", "agent": "migrator", "task": "Migrate nginx"},
		{"handle": "reviewer", "agent": "reviewer", "task": "Review nginx"},
	}}
	if res := callTool(t, cs, ToolOpen, plan, metaOf("mgr-conv", "manager", "")); !res.IsError || !strings.Contains(textOf(res), "names no person") {
		t.Fatalf("open without a person = %s", textOf(res))
	}
	res := callTool(t, cs, ToolOpen, plan, metaOf("mgr-conv", "manager", "alice"))
	if res.IsError {
		t.Fatalf("open = %s", textOf(res))
	}
	// The result sends the person to the bridge and promises nothing.
	if !strings.Contains(textOf(res), `the work goes on in the channel "Migrate nginx"`) || strings.Contains(textOf(res), "bossmode") || !strings.Contains(textOf(res), "do not promise a later report") {
		t.Fatalf("open = %s", textOf(res))
	}
	var it Item
	structured(t, res, &it)
	if it.Topic != plan["topic"] || strings.Contains(textOf(res), "words") {
		t.Fatalf("the topic = %q, the text = %s", it.Topic, textOf(res))
	}
	if it.Operator != "alice" || it.Manager == nil || it.Manager.Run != "mgr-conv" || it.Manager.Member != "default/crew-0" || len(it.Members) != 2 {
		t.Fatalf("opened = %+v", it.Bridge)
	}
	if !strings.Contains(textOf(res), "do not post that again") {
		t.Fatalf("open = %s", textOf(res))
	}
	// The channel starts with the post of the run that opened it, from the
	// open request alone, and then the line that the bridge opened.
	ch := channel(t, s, it.ID)
	want := "I opened this channel and put two agents on it.\n- **migrator-1**: Migrate nginx\n- **reviewer**: Review nginx"
	if len(ch) < 2 || ch[0].From != (From{Kind: KindAgent, Handle: HandleManager, Run: "mgr-conv"}) || ch[0].Text != want || ch[0].Seq != 1 ||
		ch[1].Event != EventOpened || ch[1].Seq != 2 {
		t.Fatalf("channel = %+v", ch)
	}
	// carol cannot operate the agents, so her manager opens nothing.
	if res := callTool(t, cs, ToolOpen, plan, metaOf("mgr-carol", "manager", "carol")); !res.IsError || !strings.Contains(textOf(res), "Forbidden") {
		t.Fatalf("open for carol = %s", textOf(res))
	}
	res = callTool(t, cs, ToolList, map[string]any{}, metaOf("mgr-conv", "manager", "alice"))
	var list listResult
	structured(t, res, &list)
	if res.IsError || len(list.Bridges) != 1 || list.Bridges[0].ID != it.ID {
		t.Fatalf("list = %s", textOf(res))
	}
	// The manager run posts as the manager, with no bridge argument.
	res = callTool(t, cs, ToolPost, map[string]any{"text": "Seats are up.", "to": []string{"operator"}}, metaOf("mgr-conv", "manager", "alice"))
	var m Message
	structured(t, res, &m)
	if res.IsError || m.From.Handle != HandleManager {
		t.Fatalf("manager post = %s", textOf(res))
	}

	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateRunning))
	s.poll(context.Background())
	args := map[string]any{"bridge": it.ID, "handle": "reviewer"}
	if res := callTool(t, cs, ToolRemoveMember, args, metaOf("mgr-conv", "manager", "alice")); !res.IsError || !strings.Contains(textOf(res), "did not delegate this for this bridge") {
		t.Fatalf("remove without delegation = %s", textOf(res))
	}
	// A seat cannot change its bridge, whatever the delegation.
	do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "alice", `{"delegation":{"members":true}}`)
	if res := callTool(t, cs, ToolRemoveMember, args, metaOf("r-rev", "reviewer", "alice")); !res.IsError || !strings.Contains(textOf(res), "only the manager") {
		t.Fatalf("remove by a seat = %s", textOf(res))
	}
	res = callTool(t, cs, ToolRemoveMember, args, metaOf("mgr-conv", "manager", "alice"))
	structured(t, res, &m)
	if res.IsError {
		t.Fatalf("remove = %s", textOf(res))
	}
	eventually(t, "the cancel reaches the run", func() bool { return len(fm.find(http.MethodPost, "/v1/runs/r-rev/cancel")) == 1 })
	if l := withEvent(channel(t, s, it.ID), EventFired); len(l) != 1 || l[0].Text != "the manager for Alice took reviewer off the channel" {
		t.Fatalf("fired = %+v", l)
	}
	res = callTool(t, cs, ToolAddMember, map[string]any{"bridge": it.ID, "handle": "reviewer-2", "agent": "reviewer", "task": "Review again"}, metaOf("mgr-conv", "manager", "alice"))
	if res.IsError {
		t.Fatalf("add = %s", textOf(res))
	}
	// The report is delegated by default; the manager closes the bridge.
	res = callTool(t, cs, ToolClose, map[string]any{"bridge": it.ID}, metaOf("mgr-conv", "manager", "alice"))
	if res.IsError || !strings.Contains(textOf(res), "report.md") {
		t.Fatalf("close = %s", textOf(res))
	}
	if n := len(fm.find(http.MethodPost, "/v1/runs/mgr-conv/messages")); n != 0 {
		t.Fatalf("the manager that closed got %d asks", n)
	}
	// Delegates answers the manager branch.
	ctx := context.Background()
	for _, c := range []struct {
		batch, action string
		want          bool
	}{
		{BatchOf(it.ID), "approve", false},
		{BatchOf(it.ID), "members", true},
		{BatchOf(it.ID), "cancel", true},
		{BatchOf(it.ID), ToolClose, true},
		{BatchOf(it.ID), "budget", false},
		{BatchOf(it.ID), "launch", false},
		{"nightly", "members", false},
		{"bridge:unknown", "members", false},
	} {
		if got, err := s.Delegates(ctx, c.batch, c.action); err != nil || got != c.want {
			t.Errorf("Delegates(%s, %s) = %v, %v", c.batch, c.action, got, err)
		}
	}
}

// A bridge is for its operator and for anyone with the operator role on
// every agent of its members.
func TestRouteAuthorization(t *testing.T) {
	a := newAgent(t, nil)
	newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	for token, want := range map[string]int{"alice": 200, "bob": 200, "carol": 403, "dave": 403, "": 401} {
		if resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID, token, ""); resp.StatusCode != want {
			t.Errorf("GET as %q = %d %s", token, resp.StatusCode, body)
		}
	}
	if resp, _ := do(t, http.MethodPost, url+"/v1/bridges/"+it.ID+"/messages", "carol", `{"text":"hi"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("carol posts = %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodDelete, url+"/v1/bridges/"+it.ID+"/members/reviewer", "carol", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("carol fires = %d", resp.StatusCode)
	}
	// dave opens a bridge of his agent: alice and bob cannot operate it.
	resp, body := do(t, http.MethodPost, url+"/v1/bridges", "dave", `{"title":"Private","members":[{"handle":"p","agent":"private","task":"Do it"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("dave opens = %d %s", resp.StatusCode, body)
	}
	var priv Item
	decodeJSON(t, body, &priv)
	for token, want := range map[string]int{"dave": 200, "alice": 403, "bob": 403} {
		if resp, _ := do(t, http.MethodGet, url+"/v1/bridges/"+priv.ID, token, ""); resp.StatusCode != want {
			t.Errorf("GET of the private bridge as %s = %d", token, resp.StatusCode)
		}
	}
	// The stream asks the same question.
	s := Of(a)
	for token, want := range map[string]bool{"alice": true, "bob": true, "carol": false} {
		ctx := fw.WithPrincipal(context.Background(), callers[token])
		if got, err := s.Visible(ctx, it.ID); err != nil || got != want {
			t.Errorf("Visible as %s = %v, %v", token, got, err)
		}
	}
	if got, err := s.Visible(context.Background(), "nope"); got || err != nil {
		t.Errorf("Visible of an unknown bridge = %v, %v", got, err)
	}
	// bob changes the bridge of alice: he operates both agents.
	if resp, body := do(t, http.MethodPatch, url+"/v1/bridges/"+it.ID, "bob", `{"title":"Migrate logrotate, part 2"}`); resp.StatusCode != http.StatusOK || !strings.Contains(body, "part 2") {
		t.Fatalf("bob patches = %d %s", resp.StatusCode, body)
	}
}

// GET /v1/bridges/{id} sums the usage of the runs of the bridge as the
// tool batch sums a job, with the partial mark.
func TestUsage(t *testing.T) {
	a := newAgent(t, nil)
	newStore(t, a, nil)
	v, _ := withFleet(t, a)
	url := serve(t, a)
	it := open(t, url)
	done := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateCompleted)
	done.Usage = fw.RunUsage{Usage: fw.Usage{InputTokens: 100, OutputTokens: 10}, Turns: 2, CostUSD: 0.5}
	done.UsageSeq = 3
	v.put(done)
	resp, body := do(t, http.MethodGet, url+"/v1/bridges/"+it.ID, "alice", "")
	var got Item
	decodeJSON(t, body, &got)
	if resp.StatusCode != http.StatusOK || got.Usage == nil || got.Usage.InputTokens != 100 || got.Usage.Turns != 2 || got.Usage.Reported != 1 || got.Usage.Final != 1 || got.Usage.Partial {
		t.Fatalf("usage = %+v", got.Usage)
	}
	v.put(workerRun("r-rev", it.ID, "reviewer", "reviewer", fw.StateRunning))
	_, body = do(t, http.MethodGet, url+"/v1/bridges/"+it.ID, "alice", "")
	decodeJSON(t, body, &got)
	if got.Usage.Missing != 1 || !got.Usage.Partial {
		t.Fatalf("usage with a working run = %+v", got.Usage)
	}
	// The list carries no usage.
	_, body = do(t, http.MethodGet, url+"/v1/bridges", "alice", "")
	if strings.Contains(body, `"usage"`) {
		t.Fatalf("the list carries usage: %s", body)
	}
}

// The configuration takes the defaults, the variables win over the file,
// and a value out of bounds stops the start with the key.
func TestConfig(t *testing.T) {
	for _, env := range []string{EnvPlacement, EnvMaxMembers, EnvMaxMessages, EnvMaxFanoutPerMinute, EnvMaxPairMessages, EnvManager} {
		t.Setenv(env, "")
	}
	c, err := Load(fw.DefaultConfig())
	if err != nil || c.Placement != PlacementQueue || c.MaxMembers != 16 || c.MaxMessages != 2000 || c.MaxFanoutPerMinute != 30 || c.MaxPairMessages != 6 || c.Manager != "" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	t.Setenv(EnvPlacement, "spawn")
	t.Setenv(EnvMaxPairMessages, "3")
	t.Setenv(EnvManager, "manager")
	if c, err = Load(fw.DefaultConfig()); err != nil || c.Placement != PlacementSpawn || c.MaxPairMessages != 3 || c.Manager != "manager" {
		t.Fatalf("variables = %+v, %v", c, err)
	}
	t.Setenv(EnvPlacement, "elsewhere")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), "workbench.bridges.placement") {
		t.Fatalf("a bad placement = %v", err)
	}
	t.Setenv(EnvPlacement, "")
	t.Setenv(EnvMaxMembers, "1000")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), "workbench.bridges.maxMembers") {
		t.Fatalf("too many members = %v", err)
	}
	t.Setenv(EnvMaxMembers, "x")
	if _, err := Load(fw.DefaultConfig()); err == nil || !strings.Contains(err.Error(), EnvMaxMembers) {
		t.Fatalf("not a number = %v", err)
	}
	t.Setenv(EnvMaxMembers, "")
	cfg := fw.DefaultConfig()
	cfg.Bus.Driver = "redis"
	if ok, line := Status(cfg); !ok || !strings.Contains(line, "off") {
		t.Fatalf("status on redis = %v %s", ok, line)
	}
}

// On a bus that keeps no store, the service serves nothing.
func TestOffOnAnotherBus(t *testing.T) {
	a := newAgent(t, func(c *fw.Config) { c.Bus.Driver = "redis" })
	closer, err := Start(context.Background(), a)
	if err != nil || closer != nil || Of(a) != nil {
		t.Fatalf("Start on redis = %v, %v", closer, err)
	}
}

// The service opens the stores on the mem bus, watches the view, and reads
// it at once on Wake; its end detaches the store.
func TestServiceWatches(t *testing.T) {
	a := newAgent(t, nil)
	closer, err := Start(context.Background(), a)
	if err != nil || closer == nil {
		t.Fatalf("Start = %v, %v", closer, err)
	}
	s := Of(a)
	if s == nil || !strings.Contains(s.Describe(), "memory") {
		t.Fatalf("store = %v", s)
	}
	v, _ := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	rec, err := s.open(context.Background(), OpenSpec{Title: "Watched", Members: []SeatSpec{{Handle: "migrator-1", Agent: "migrator", Task: "Migrate"}}}, callers["alice"], nil)
	if err != nil {
		t.Fatal(err)
	}
	v.put(workerRun("r-mig", rec.ID, "migrator-1", "migrator", fw.StateRunning))
	s.Wake()
	eventually(t, "the watch posts the start", func() bool { return len(withEvent(channel(t, s, rec.ID), EventStarted)) == 1 })
	if err := closer.Close(); err != nil || Of(a) != nil {
		t.Fatalf("Close = %v, store %v", err, Of(a))
	}
}

// The list of a person holds the bridges of the person and of the teams
// of the person. An admin of the process lists every bridge that it can
// use, also a bridge of the static token, which has no team.
func TestListAdmin(t *testing.T) {
	a := newAgent(t, func(c *fw.Config) {
		c.Access.Admins = []string{"carol", "kind:token"}
	})
	newStore(t, a, nil)
	url := serve(t, a)
	it := open(t, url)
	resp, body := do(t, http.MethodPost, url+"/v1/bridges", "dave", `{"title":"Private","members":[{"handle":"p","agent":"private","task":"Do it"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("dave opens = %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPost, url+"/v1/bridges", "fleet", `{"title":"Seeded","members":[{"handle":"m","agent":"migrator","task":"Seed"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the token opens = %d %s", resp.StatusCode, body)
	}
	list := func(token string) []string {
		t.Helper()
		resp, body := do(t, http.MethodGet, url+"/v1/bridges", token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list as %s = %d %s", token, resp.StatusCode, body)
		}
		var out struct {
			Bridges []Item `json:"bridges"`
		}
		decodeJSON(t, body, &out)
		var titles []string
		for _, b := range out.Bridges {
			titles = append(titles, b.Title)
		}
		return titles
	}
	// bob shares team-a with alice and dave and operates the agents of
	// alice alone. The token has no team and sees its own bridge.
	if got := list("bob"); strings.Join(got, ",") != it.Title {
		t.Errorf("bob lists %v", got)
	}
	if got := list("alice"); strings.Join(got, ",") != it.Title {
		t.Errorf("alice lists %v", got)
	}
	// carol has no team and no role, and is an admin: she lists all three.
	got := list("carol")
	if len(got) != 3 || !strings.Contains(strings.Join(got, ","), "Seeded") || !strings.Contains(strings.Join(got, ","), "Private") {
		t.Errorf("carol lists %v", got)
	}
}

// The post of the opener names each agent with the first line of its task,
// and writes a small count in words.
func TestOpenerText(t *testing.T) {
	one := openerText([]Member{{Handle: "scribe", Task: "\n  Draft the release notes of 2.4.\nUse the merged pull requests."}})
	if one != "I opened this channel and put one agent on it.\n- **scribe**: Draft the release notes of 2.4." {
		t.Fatalf("one = %q", one)
	}
	many := make([]Member, 12)
	for i := range many {
		many[i] = Member{Handle: fmt.Sprintf("a-%d", i)}
	}
	if got := openerText(many); !strings.HasPrefix(got, "I opened this channel and put 12 agents on it.\n- **a-0**\n") {
		t.Fatalf("many = %q", got)
	}
}
