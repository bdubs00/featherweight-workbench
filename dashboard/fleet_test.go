package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// stubFleet is a fleet with fixed runs of another member.
type stubFleet struct {
	runs []*fw.Run
	err  error
}

func (stubFleet) Name() string { return "here" }

func (stubFleet) Members(context.Context) ([]fleet.Member, error) { return nil, nil }

func (s stubFleet) Runs(context.Context) ([]*fw.Run, error) { return s.runs, s.err }

func (s stubFleet) Run(_ context.Context, id string) (*fw.Run, error) {
	for _, r := range s.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fw.ErrRunNotFound
}

func (stubFleet) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	return nil, fleet.ErrNoRoute
}

func ids(runs []*fw.Run) string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

func TestMembersText(t *testing.T) {
	text := MembersText("here", []fleet.Member{
		{Name: "here", Self: true, State: fleet.MemberLive, AccessMode: fw.AccessModeRules, Agents: []fleet.RecordAgent{{Name: "reviewer", Role: "owner"}}, Active: 1},
		{Name: "there", State: fleet.MemberQuiet, LastSeen: time.Now().Add(-3 * time.Minute), AccessMode: fw.AccessModeExternal, CanForward: true, Agents: []fleet.RecordAgent{{Name: "deployer", Role: "operator"}}, InputRequired: 2},
		{Name: "far", State: fleet.MemberStopped, LastSeen: time.Now().Add(-50 * time.Hour)},
		{Name: "full", State: fleet.MemberQuiet, LastSeen: time.Now().Add(-10 * time.Second), BlockedReason: "storage unavailable: no space left on device"},
	})
	for _, want := range []string{
		"This process is the fleet member here.",
		"- here (this process): live, access rules; agents reviewer (you are owner); 1 active run, 0 waiting for input",
		"- there: quiet, heard 3m ago, access external; agents deployer (you are operator); 0 active runs, 2 waiting for input; this process can act on its runs",
		"- far: stopped, heard 2d ago; no agent you can see",
		"- full: quiet, heard 10s ago; blocked by a storage failure (storage unavailable: no space left on device), so the front suspends its Task, and the next request restarts it; no agent you can see",
		"read-only from here",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if got := MembersText("here", nil); !strings.Contains(got, "You can see no member.") {
		t.Errorf("empty = %s", got)
	}
}

func TestPageHasTheFleet(t *testing.T) {
	for _, want := range []string{"/v1/fleet", `call('fleet')`, `S.me.links.metrics`, `'/v1/home'`, `data-view="others"`, "Runs by others", "?member="} {
		if !bytes.Contains(Page(), []byte(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
}
