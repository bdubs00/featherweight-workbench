package knowhow

// This file serves GET /v1/knowhow/runs (docs/KNOWHOW.md sections 4.7 and
// 5.3): the search of recall across Tasks. The front searches the archive
// for the runs of one agent and one principal, with an exact match of the
// principal, so a run never reads the runs of another person. Each field
// is cut to 2 KB. The kernel keeps every hit untrusted.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// Bounds of the search of runs.
const (
	runsDefault  = 20
	runsMax      = 50
	runFieldCut  = 2048
	maxQueryRune = 256
)

// RunHit is one run of the search: its id, title, input, output, the time
// it started, and its outcome, each text cut to 2 KB.
type RunHit struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Input   string    `json:"input"`
	Output  string    `json:"output"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
}

// words splits a query into the lower-case runs of letters and digits.
func words(q string) []string {
	return strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// Runs searches the archive for the runs of the agent whose principal is
// exactly principal, newest first, at most limit. A query keeps the runs
// whose title, input, or output hold every word of q.
func (s *Service) Runs(ctx context.Context, agent, principal, q string, limit int) ([]RunHit, error) {
	if err := CheckAgent(agent); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = runsDefault
	}
	want := words(q)
	var list []RunHit
	for _, e := range s.archive(ctx) {
		r := e.Run
		if r.Agent != agent || r.Principal == "" || r.Principal != principal {
			continue
		}
		if len(want) > 0 {
			hay := strings.ToLower(r.Title + "\n" + r.Input + "\n" + r.Output)
			all := true
			for _, w := range want {
				if !strings.Contains(hay, w) {
					all = false
					break
				}
			}
			if !all {
				continue
			}
		}
		outcome := string(r.State)
		if r.Outcome != nil && r.Outcome.Verdict != "" {
			outcome += ", graded " + r.Outcome.Verdict
		}
		list = append(list, RunHit{ID: r.ID, Title: cut(r.Title, runFieldCut), Input: cut(r.Input, runFieldCut), Output: cut(r.Output, runFieldCut),
			At: r.CreatedAt, Outcome: cut(outcome, runFieldCut)})
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].At.Equal(list[j].At) {
			return list[i].At.After(list[j].At)
		}
		return list[i].ID > list[j].ID
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

// runs answers GET /v1/knowhow/runs?agent=&principal=&q=&limit=. A worker
// names any principal; a person reads the runs of their own principal,
// and an operator of the agent also the runs of its workload principals.
func (rt *routes) runs(w http.ResponseWriter, r *http.Request, s *Service) {
	ctx := onBehalf(r)
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	limit, ok := limitParam(w, r, runsDefault, runsMax)
	if !ok {
		return
	}
	q := r.URL.Query()
	rg, err := s.rightsOn(ctx, agent)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	principal := strings.TrimSpace(q.Get("principal"))
	if principal == "" && !rg.c.trusted {
		principal = rg.c.subject
	}
	if principal == "" {
		native.WriteProblem(w, r, http.StatusBadRequest, "name the principal of the runs with ?principal=")
		return
	}
	if !rg.read(Scope{Kind: ScopePrincipal, Principal: principal}) {
		rt.fail(w, r, ErrHidden)
		return
	}
	query := q.Get("q")
	if len([]rune(query)) > maxQueryRune {
		native.WriteProblem(w, r, http.StatusBadRequest, "q holds at most 256 characters")
		return
	}
	hits, err := s.Runs(ctx, agent, principal, query, limit)
	if err != nil {
		rt.fail(w, r, err)
		return
	}
	native.WriteJSON(w, http.StatusOK, map[string]any{"agent": agent, "runs": hits})
}
