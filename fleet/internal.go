package fleet

// This file marks the runs that the front starts for itself, such as the
// check run of a duty, which calls a tool of an agent and no model, and
// the run that writes the check of a duty. The lists of runs of the
// workbench leave such a run out (Listed): the MCP tool runs_list, GET
// /v1/fleet/runs, the home of a person, the stream of bossmode, and the
// counts of the busy runs of a member. The front then waits for such a run
// and reads its whole output (Await).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// MetaInternal is the metadata key of a run that the front starts for
// itself and not as the work of a person, such as the run that writes the
// check of a duty. Its value names the purpose, such as
// "duty-write-check:<name>". Only the front sets it.
const MetaInternal = "workbench.internal"

// metaVerdict is the metadata key that names the approval of a verdict
// run of the manager for a duty (away.MetaVerdict). A verdict run of
// before 2026-10-24 carries it without MetaInternal, and the archive and
// the ledgers keep such runs, so the key marks them as runs of the front
// too.
const metaVerdict = "workbench.dutyVerdict"

// Internal returns the purpose of a run that the front starts for itself:
// the value of MetaInternal, "duty-verdict" for a verdict run that names
// no purpose, and "" for any other run.
func Internal(r *fw.Run) string {
	if r == nil {
		return ""
	}
	if v := r.Metadata[MetaInternal]; v != "" {
		return v
	}
	if r.Metadata[metaVerdict] != "" {
		return "duty-verdict"
	}
	return ""
}

// Listed reports whether the lists of runs of the workbench show a run.
// They leave out a check run of the kernel (fw.RunKindCheck) and a run
// that the front starts for itself (Internal), such as the verdict run of
// a duty and the run that writes the check of a duty.
func Listed(r *fw.Run) bool {
	return r != nil && r.Kind() == "" && Internal(r) == ""
}

// ListedOnly returns the runs that the lists show (Listed), in their
// order. It keeps the slice when every run is listed.
func ListedOnly(runs []*fw.Run) []*fw.Run {
	for i, r := range runs {
		if Listed(r) {
			continue
		}
		out := append(make([]*fw.Run, 0, len(runs)-1), runs[:i]...)
		for _, r := range runs[i+1:] {
			if Listed(r) {
				out = append(out, r)
			}
		}
		return out
	}
	return runs
}

// awaitPoll is how often Await reads the run that it waits for.
const awaitPoll = 200 * time.Millisecond

// Await waits until the run id ends or waits for a person
// (fw.StateInputRequired), and returns it with its whole output. The front
// put the request of the run on a queue, so a run that no process holds
// yet waits in the queue. Await reads the run from this process, else
// from the view of the fleet, and once the run settled it reads the whole
// run from the member that holds it, with the static token of the fleet,
// because a record of the view cuts the output. It reads as the front
// itself, whatever the principal of ctx, and it stops when ctx ends.
func Await(ctx context.Context, a *fw.Agent, id string) (*fw.Run, error) {
	fctx := fw.WithPrincipal(ctx, nil)
	tick := time.NewTicker(awaitPoll)
	defer tick.Stop()
	for {
		r, err := settled(fctx, a, id)
		if err != nil || r != nil {
			return r, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("fleet: wait for the run %s: %w", id, context.Cause(ctx))
		case <-tick.C:
		}
	}
}

// settledState reports whether a run ended or waits for a person.
func settledState(r *fw.Run) bool {
	return r.State.Terminal() || r.State == fw.StateInputRequired
}

// settled returns the run id, whole, once it ended or waits for a person,
// and nil while it works or no process holds it yet.
func settled(ctx context.Context, a *fw.Agent, id string) (*fw.Run, error) {
	r, err := a.Get(ctx, id)
	switch {
	case err == nil && settledState(r):
		return r, nil
	case err == nil:
		return nil, nil
	case !errors.Is(err, fw.ErrRunNotFound):
		return nil, err
	}
	v := Of(a)
	if v == nil {
		return nil, nil
	}
	r, err = v.Run(ctx, id)
	switch {
	case errors.Is(err, fw.ErrRunNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case !settledState(r):
		return nil, nil
	}
	tctx, err := asFront(ctx, a)
	if err != nil {
		return nil, err
	}
	return MemberRun(tctx, v, r.Member, id)
}

// asFront returns ctx with the static token of the fleet as its
// credential, for a request of the front itself to a member.
func asFront(ctx context.Context, a *fw.Agent) (context.Context, error) {
	tok, err := Token(ctx, a.Config())
	if err != nil {
		return nil, err
	}
	return fw.WithCredential(fw.WithPrincipal(ctx, nil), "Bearer "+tok), nil
}

// CancelRun cancels the run id for the front itself: in this process, else
// at the member that holds it, with the static token of the fleet. A run
// that no process holds, such as a request that still waits in a queue,
// gives an error that wraps fw.ErrRunNotFound.
func CancelRun(ctx context.Context, a *fw.Agent, id string) error {
	fctx := fw.WithPrincipal(ctx, nil)
	err := a.Cancel(fctx, id)
	if err == nil || !errors.Is(err, fw.ErrRunNotFound) {
		return err
	}
	v := Of(a)
	if v == nil {
		return err
	}
	r, err := v.Run(fctx, id)
	if err != nil {
		return err
	}
	tctx, err := asFront(fctx, a)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(tctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/cancel", http.NoBody)
	if err != nil {
		return fmt.Errorf("fleet: build the cancel of the run %s: %w", id, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.Forward(tctx, r.Member, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= http.StatusMultipleChoices {
		return errorOf(r.Member, resp.StatusCode, b)
	}
	return nil
}
