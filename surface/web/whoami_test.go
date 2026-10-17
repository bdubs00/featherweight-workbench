package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/dashboard"
	"github.com/bdubs00/featherweight-workbench/teams"
)

// GET /v1/whoami adds the teams of the caller, and GET /v1/home/others
// lists the top-level runs of the people outside those teams. A run keeps
// the teams of the person who started it through the run annotator.
func TestWhoamiTeamsAndOthers(t *testing.T) {
	t.Setenv(teams.EnvTeams, "")
	team := tokenMap{
		"alice-token": {Subject: "alice", Kind: fw.KindUser, Groups: []string{"pay"}},
		"bob-token":   {Subject: "bob", Kind: fw.KindUser, Groups: []string{"pay", "ops"}},
		"carol-token": {Subject: "carol", Kind: fw.KindUser, Groups: []string{"ops"}},
	}
	a := newAgent(t, newScript(endTurn("a"), endTurn("b"), endTurn("c")).model(), fw.WithAuthn(team), fw.WithRunAnnotators(teams.Annotate))
	srv := serve(t, a)
	for _, tok := range []string{"alice-token", "bob-token", "carol-token"} {
		resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true}`, "Authorization", "Bearer "+tok)
		if r := decodeRun(t, resp, b, http.StatusOK); r.Metadata[teams.MetaKey] == "" {
			t.Fatalf("the run of %s keeps no teams: %v", tok, r.Metadata)
		}
	}

	resp, b := do(t, "GET", srv.URL+"/v1/whoami", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("whoami without a credential = %d: %s", resp.StatusCode, b)
	}
	var who dashboard.Whoami
	resp, b = do(t, "GET", srv.URL+"/v1/whoami", "", "Authorization", "Bearer alice-token")
	if err := json.Unmarshal(b, &who); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("whoami = %d %s: %v", resp.StatusCode, b, err)
	}
	if who.Principal.Subject != "alice" || !slices.Equal(who.Teams, []string{"pay"}) || who.Roles["default"] == "" {
		t.Fatalf("whoami = %s", b)
	}

	var others struct {
		Runs []*fw.Run `json:"runs"`
	}
	resp, b = do(t, "GET", srv.URL+"/v1/home/others", "", "Authorization", "Bearer alice-token")
	if err := json.Unmarshal(b, &others); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("others = %d %s: %v", resp.StatusCode, b, err)
	}
	if len(others.Runs) != 1 || others.Runs[0].Principal != "carol" {
		t.Fatalf("others = %s", b)
	}
}
