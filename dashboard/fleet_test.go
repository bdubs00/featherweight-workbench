package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stubFleet is a fleet with fixed runs of another member.
type stubFleet struct {
	runs []*fw.Run
	err  error
}

func (stubFleet) Name() string { return "here" }

func (stubFleet) Members(context.Context) ([]fw.FleetMember, error) { return nil, nil }

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
	return nil, fw.ErrFleetNoRoute
}

func ids(runs []*fw.Run) string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return strings.Join(out, ",")
}

func TestMembersText(t *testing.T) {
	text := MembersText("here", []fw.FleetMember{
		{Name: "here", Self: true, State: fw.MemberLive, AccessMode: fw.AccessModeRules, Agents: []fw.FleetAgent{{Name: "reviewer", Role: "owner"}}, Active: 1},
		{Name: "there", State: fw.MemberQuiet, LastSeen: time.Now().Add(-3 * time.Minute), AccessMode: fw.AccessModeExternal, CanForward: true, Agents: []fw.FleetAgent{{Name: "deployer", Role: "operator"}}, InputRequired: 2},
		{Name: "far", State: fw.MemberStopped, LastSeen: time.Now().Add(-50 * time.Hour)},
	})
	for _, want := range []string{
		"This process is the fleet member here.",
		"- here (this process): live, access rules; agents reviewer (you are owner); 1 active run, 0 waiting for input",
		"- there: quiet, heard 3m ago, access external; agents deployer (you are operator); 0 active runs, 2 waiting for input; this process can act on its runs",
		"- far: stopped, heard 2d ago; no agent you can see",
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
	for _, want := range []string{"/v1/fleet", `call('fleet')`, `data-view="operator"`, `'/v1/home'`, `data-view="others"`, "Runs by others", "?member="} {
		if !bytes.Contains(Page(), []byte(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
}
