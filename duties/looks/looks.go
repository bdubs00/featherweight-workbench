// Package looks gives the check of a duty its look through a tool of the
// agent (docs/ARCHITECTURE.md section 24.3). A check with
// checkFirst.look.tool, such as a query of a ServiceNow queue, cannot run
// in the front: the worker of the agent holds the tool, its credential,
// its egress, and its policy. So the front puts a check run on the queue
// of the pool of the agent: a run request with one step of the kernel
// (fw.Check), which calls the tool with the policy of the agent and calls
// no model. The front waits for the run and reads its output, the
// document in which the check finds its items.
//
// The package registers the look tool with duties.RegisterLook in init. A
// binary turns it on with a blank import.
package looks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/duties"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

func init() {
	duties.RegisterLook(duties.LookNameTool, (&looker{enqueue: fleet.EnqueueFor, await: fleet.Await, cancel: fleet.CancelRun}).look)
}

// Wait is the longest wait for a check run: the check timeout of its
// calls, and as long again for the queue and for a worker that wakes.
const Wait = 2 * duties.CheckTimeout

// PurposeCheck starts the value of fleet.MetaInternal of a check run,
// "duty-check:<name>".
const PurposeCheck = "duty-check"

// looker is the look tool with the seams of the fleet, which a test
// replaces.
type looker struct {
	enqueue func(ctx context.Context, a *fw.Agent, agent, pool string, reqs []fw.RunRequest) (string, int, error)
	await   func(ctx context.Context, a *fw.Agent, id string) (*fw.Run, error)
	cancel  func(ctx context.Context, a *fw.Agent, id string) error
	wait    time.Duration // Wait when zero
}

// look reads the document of the check of a duty through the tool of its
// agent: it puts one check run on the queue of the pool of the agent,
// waits for it, and returns the output of its step. A failed run, an
// error result of the tool, and a run that does not end in time are
// errors with the reason in words.
func (l *looker) look(ctx context.Context, a *fw.Agent, d *duties.Duty) ([]byte, error) {
	c := d.CheckFirst
	if c == nil || strings.TrimSpace(c.Look.Tool) == "" {
		return nil, errors.New("the check names no tool")
	}
	req, err := Request(d)
	if err != nil {
		return nil, err
	}
	wait := l.wait
	if wait <= 0 {
		wait = Wait
	}
	ctx, stop := context.WithTimeout(ctx, wait)
	defer stop()
	_, sent, err := l.enqueue(ctx, a, d.Agent, d.Pool, []fw.RunRequest{req})
	if err == nil && sent == 0 {
		err = errors.New("no request went out")
	}
	if err != nil {
		return nil, fmt.Errorf("the queue of the agent %s did not take the check: %s", d.Agent, words(err))
	}
	r, err := l.await(ctx, a, req.ID)
	if err != nil {
		if ctx.Err() != nil {
			// The run that did not end in time stops, so it does not
			// call the tool after the front gave up.
			_ = l.cancel(context.WithoutCancel(ctx), a, req.ID)
			return nil, fmt.Errorf("the check run %s did not end within %s", req.ID, wait)
		}
		return nil, fmt.Errorf("the front did not read the check run %s: %s", req.ID, words(err))
	}
	switch r.State {
	case fw.StateCompleted:
	case fw.StateInputRequired:
		_ = l.cancel(context.WithoutCancel(ctx), a, req.ID)
		return nil, fmt.Errorf("the tool %s waits for a person, and nobody answers a check", c.Look.Tool)
	case fw.StateFailed:
		return nil, fmt.Errorf("the check run %s failed: %s", r.ID, orWords(r.Error, "no reason"))
	default:
		return nil, fmt.Errorf("the check run %s ended %s", r.ID, r.State)
	}
	return Document(c.Look.Tool, r.Output)
}

// Request returns the run request of the check of a duty: one step that
// calls the tool of the check with its input, with the time limit and the
// output limit of a check, as the principal that the runs of the duty act
// as. The metadata names the duty in fleet.MetaInternal, so the lists of
// runs leave the run out, and the run carries no batch, so the history
// of the duty never takes it for a run of the duty.
func Request(d *duties.Duty) (fw.RunRequest, error) {
	c := d.CheckFirst
	input := json.RawMessage(`{}`)
	if len(c.Look.Input) > 0 {
		b, err := json.Marshal(c.Look.Input)
		if err != nil {
			return fw.RunRequest{}, fmt.Errorf("the input of the tool %s does not encode: %w", c.Look.Tool, err)
		}
		input = b
	}
	principal, kind := "duty:"+d.Name, fw.KindWorkload
	if d.RunAs == duties.RunAsOwner && d.Owner != "" {
		principal, kind = d.Owner, fw.KindUser
	}
	return fw.RunRequest{
		ID:    fw.NewID(),
		Agent: d.Agent,
		Check: &fw.Check{
			Steps:    []fw.CheckStep{{Tool: c.Look.Tool, Input: input}},
			Timeout:  duties.CheckTimeout,
			MaxBytes: duties.MaxCheckBytes,
		},
		Metadata: map[string]string{
			fleet.MetaInternal: PurposeCheck + ":" + d.Name,
			fw.MetaTitle:       "Check of the duty " + d.Name,
		},
		Principal:     principal,
		PrincipalKind: kind,
	}, nil
}

// Document returns the document of a check from the output of a check
// run (fw.CheckOutput): the output of its one step. A step whose output is
// a JSON string that holds JSON, as a tool that answers JSON in a text
// does, gives that JSON. An error result of the tool, and an output that
// the kernel cut, are errors.
func Document(tool, output string) ([]byte, error) {
	var out fw.CheckOutput
	if err := json.Unmarshal([]byte(output), &out); err != nil {
		return nil, fmt.Errorf("the output of the check run is not the output of a check: %v", err)
	}
	if len(out.Results) == 0 {
		return nil, fmt.Errorf("the check run gave no result of the tool %s", tool)
	}
	res := out.Results[0]
	if res.IsError {
		return nil, fmt.Errorf("the tool %s answered an error: %s", tool, cut(textOf(res.Output), 300))
	}
	if res.Cut {
		return nil, fmt.Errorf("the answer of the tool %s is longer than %d KB, so the check does not read it", tool, duties.MaxCheckBytes>>10)
	}
	doc := bytes.TrimSpace(res.Output)
	var s string
	if json.Unmarshal(doc, &s) == nil && json.Valid([]byte(strings.TrimSpace(s))) {
		doc = []byte(strings.TrimSpace(s))
	}
	return doc, nil
}

// textOf returns the text of a result: a JSON string as its text, and any
// other value as its JSON.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// cut shortens a text to n bytes at a rune boundary, with an ellipsis.
func cut(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// words returns the words of an error without the names of the packages
// and the kinds of error in front.
func words(err error) string {
	s := err.Error()
	for _, p := range []string{"fleet: ", fw.ErrInvalidRequest.Error() + ": "} {
		s = strings.ReplaceAll(s, p, "")
	}
	return strings.TrimSuffix(s, ".")
}

// orWords returns s, or alt when s is empty.
func orWords(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}
