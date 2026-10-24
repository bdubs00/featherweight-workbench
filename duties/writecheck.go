package duties

// This file holds what the page offers in New duty and the write of a
// check in words (docs/ARCHITECTURE.md section 22.7). Nobody writes a
// check by hand: a person says what the check looks for, such as "a new
// P1 or P2 in the Platform Ops queue", and the agent of the duty, which
// knows its own tools, proposes the record checkFirst. The front reads the
// answer strictly, checks it as a dry run does, runs it once as a test,
// and reads it back in words from the record, not from the words of the
// model.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// OptionsAnswer is what the page can offer in New duty: the secrets that a
// check or a webhook can name, with the hosts of each and never a value,
// and the kinds of event, the actions, and the looks that this front
// serves now. Each list is sorted.
type OptionsAnswer struct {
	Secrets []SecretOption `json:"secrets"`
	Events  []string       `json:"events"`
	Actions []string       `json:"actions"`
	Looks   []string       `json:"looks"`
}

// SecretOption is one secret of workbench.duties.secrets: the name of the
// variable and the hosts to which a check can send it.
type SecretOption struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
}

// Options returns what the page can offer: the secrets of the rules of
// the operator, by name and hosts, and what Offered names.
func (s *Service) Options() OptionsAnswer {
	ans := OptionsAnswer{Secrets: []SecretOption{}}
	for _, r := range s.cfg.Secrets {
		hosts := append([]string{}, r.Hosts...)
		ans.Secrets = append(ans.Secrets, SecretOption{Name: r.Name, Hosts: hosts})
	}
	ans.Events, ans.Actions, ans.Looks = Offered()
	return ans
}

// Bounds of a write of a check.
const (
	// WriteCheckTimeout is the longest wait for the answer of the agent.
	// The front then cancels its run.
	WriteCheckTimeout = 120 * time.Second
	// maxWords bounds the words of a person for a check.
	maxWords = 2000
	// PurposeWriteCheck starts the value of fleet.MetaInternal of the run
	// that writes a check, "duty-write-check:<name>".
	PurposeWriteCheck = "duty-write-check"
)

// ErrNoAnswer means that the agent did not answer within
// WriteCheckTimeout, so the front canceled its run. A route answers 504.
var ErrNoAnswer = errors.New("duties: the agent did not answer in time")

// CheckError is a check that the agent did not write, or wrote and that
// does not work, with the reason in words. CheckFirst and Readback hold
// the record that the agent wrote, when it wrote one that reads. A route
// answers 422.
type CheckError struct {
	Reason     string
	CheckFirst *CheckFirst
	Readback   string
}

func (e *CheckError) Error() string { return e.Reason }

// WriteCheckBody is the body of a write of a check: the words of a person,
// and the agent of a duty that does not exist yet. For a saved duty, the
// agent of the duty wins.
type WriteCheckBody struct {
	Words string `json:"words"`
	Agent string `json:"agent,omitempty"`
}

// WriteCheckAnswer is the check that the agent wrote: the record, its
// readback in words, what a test of it finds now, which is null when the
// front cannot run the look yet, and the run that wrote it.
type WriteCheckAnswer struct {
	CheckFirst *CheckFirst `json:"checkFirst"`
	Readback   string      `json:"readback"`
	Check      *TestAnswer `json:"check"`
	Run        string      `json:"run"`
}

// WriteCheck asks the agent of a duty to turn the words of the caller in
// ctx into the record checkFirst. The caller needs the owner role on the
// agent, as for a write of the duty. The run is a run of the agent itself,
// with its profile, its model, and its budget, as the caller, with fixed
// instructions that describe the record, the looks and the secrets that
// this front offers, and the tools of the agent. An agent without a budget
// does not run, because a paid default would be a silent cost. The front
// reads the answer strictly, checks the record as a dry run of the duty
// does, tests it when the front can run its look, and answers it with
// its readback. It returns a *CheckError for an answer that does not
// work, ErrNoAnswer after WriteCheckTimeout, and a *ProblemsError for a
// body that does not fit. The run carries fleet.MetaInternal, so the lists
// of runs leave it out.
func (s *Service) WriteCheck(ctx context.Context, name string, body WriteCheckBody) (WriteCheckAnswer, error) {
	words := strings.TrimSpace(body.Words)
	switch {
	case words == "":
		return WriteCheckAnswer{}, &ProblemsError{Problems: []string{problem("words", "Say in words what the check looks for, such as \"a new P1 or P2 in the Platform Ops queue\".")}}
	case len(words) > maxWords:
		return WriteCheckAnswer{}, &ProblemsError{Problems: []string{problem("words", "The words hold more than %d bytes.", maxWords)}}
	}
	if s.passive {
		return WriteCheckAnswer{}, ErrNoStarts
	}
	if err := CheckName(name); err != nil && name != DraftName {
		return WriteCheckAnswer{}, &ProblemsError{Problems: []string{problem("name", "%s.", capital(err.Error()))}}
	}
	if s.fileOf(name) != nil {
		return WriteCheckAnswer{}, readOnly(name)
	}
	d := Duty{Name: name}
	st, err := s.st.get(ctx, name)
	switch {
	case err == nil:
		if err := s.allowed(ctx, st.Duty.Agent, fw.ActionAgentEdit); err != nil {
			if errors.Is(err, ErrHidden) {
				return WriteCheckAnswer{}, hidden(name)
			}
			return WriteCheckAnswer{}, err
		}
		d = st.Duty
	case errors.Is(err, ErrNotFound):
		d.Agent = strings.TrimSpace(body.Agent)
		if d.Agent == "" {
			return WriteCheckAnswer{}, &ProblemsError{Problems: []string{problem("agent", "Name the agent of the new duty. It writes the check with its own tools.")}}
		}
		if err := s.allowed(ctx, d.Agent, fw.ActionAgentEdit); err != nil {
			if errors.Is(err, ErrHidden) {
				return WriteCheckAnswer{}, fmt.Errorf("%w: no agent that you can view is named %s", ErrHidden, d.Agent)
			}
			return WriteCheckAnswer{}, err
		}
	default:
		return WriteCheckAnswer{}, err
	}
	req, err := s.writeRequest(ctx, &d, words)
	if err != nil {
		return WriteCheckAnswer{}, err
	}
	by, _ := caller(ctx)
	s.log.Info("duties: an agent writes a check", "duty", name, "agent", d.Agent, "run", req.ID, "by", by)
	front := fw.WithPrincipal(ctx, nil)
	_, sent, err := s.enqueue(front, d.Agent, d.Pool, []fw.RunRequest{req})
	if err == nil && sent == 0 {
		err = errors.New("no request went out")
	}
	if err != nil {
		return WriteCheckAnswer{}, &CheckError{Reason: fmt.Sprintf("The queue of the agent %s did not take the run: %s.", d.Agent, reasonOf(err))}
	}
	wctx, cancel := context.WithTimeout(ctx, s.writeWait)
	r, err := s.await(wctx, req.ID)
	cancel()
	if err != nil {
		if stopErr := s.stopRun(context.WithoutCancel(ctx), req.ID); stopErr != nil && !errors.Is(stopErr, fw.ErrRunNotFound) {
			s.log.Warn("duties: the run that writes a check did not stop", "duty", name, "run", req.ID, "error", stopErr)
		}
		if ctx.Err() == nil && wctx.Err() != nil {
			return WriteCheckAnswer{}, fmt.Errorf("%w: the agent %s did not answer within %s, so the front canceled its run %s", ErrNoAnswer, d.Agent, durText(s.writeWait), req.ID)
		}
		return WriteCheckAnswer{}, err
	}
	switch r.State {
	case fw.StateCompleted:
	case fw.StateInputRequired:
		_ = s.stopRun(context.WithoutCancel(ctx), req.ID)
		return WriteCheckAnswer{}, &CheckError{Reason: fmt.Sprintf("The agent %s asked a person to allow a tool while it wrote the check, so the front canceled its run %s. Say the check in other words, or give the agent a tool that only reads.", d.Agent, r.ID)}
	default:
		return WriteCheckAnswer{}, &CheckError{Reason: fmt.Sprintf("The run %s of the agent %s ended %s and wrote no check: %s.", r.ID, d.Agent, r.State, strings.TrimSuffix(orWords(r.Error, "no reason"), "."))}
	}
	c, err := parseCheck(r.Output)
	if err != nil {
		return WriteCheckAnswer{}, &CheckError{Reason: fmt.Sprintf("The answer of the agent %s is not one JSON object of a check: %s.", d.Agent, strings.TrimSuffix(err.Error(), "."))}
	}
	return s.proveCheck(ctx, d, c, r.ID)
}

// proveCheck checks a record that the agent wrote as a dry run checks a
// duty, tests it when the front can run its look, and answers it with its
// readback.
func (s *Service) proveCheck(ctx context.Context, d Duty, c *CheckFirst, run string) (WriteCheckAnswer, error) {
	d.CheckFirst = c
	zone := ""
	if d.When.Schedule != nil {
		zone = d.When.Schedule.TimeZone
	}
	d.normalize(zone)
	c = d.CheckFirst
	problems := append(c.problems(), s.checkContextProblems(ctx, c)...)
	if len(problems) > 0 {
		return WriteCheckAnswer{}, &CheckError{Reason: "The check that the agent wrote does not fit: " + strings.Join(problems, " "), CheckFirst: c}
	}
	ans := WriteCheckAnswer{CheckFirst: c, Readback: checkWords(&d, c), Run: run}
	if c.Look.Tool != "" && lookOf(LookNameTool) == nil {
		return ans, nil
	}
	t, err := s.testCheck(ctx, &d)
	if err != nil {
		return WriteCheckAnswer{}, &CheckError{Reason: "The check that the agent wrote failed its test: " + strings.TrimSuffix(err.Error(), ".") + ".", CheckFirst: c, Readback: ans.Readback}
	}
	ans.Check = t
	return ans, nil
}

// writeRequest returns the run request that asks the agent of a duty for
// its check: the words of the person as the input, the fixed instructions,
// the budget of the profile of the agent with a deadline of
// WriteCheckTimeout at most, and the caller in ctx as its principal. A
// profile without a budget gets a *CheckError.
func (s *Service) writeRequest(ctx context.Context, d *Duty, words string) (fw.RunRequest, error) {
	front := fw.WithPrincipal(ctx, nil)
	place, err := fleet.PlaceOf(front, s.a, d.Agent, d.Pool)
	if err != nil {
		return fw.RunRequest{}, &CheckError{Reason: fmt.Sprintf("The front did not find the profile of the agent %s: %s.", d.Agent, reasonOf(err))}
	}
	prof, ok := place.Profile(d.Agent)
	if !ok {
		return fw.RunRequest{}, &CheckError{Reason: fmt.Sprintf("The front did not find the profile of the agent %s.", d.Agent)}
	}
	b := prof.Budget
	if b == nil || (b.MaxCostUSD <= 0 && b.MaxTurns <= 0 && b.MaxTokens <= 0 && b.MaxToolCalls <= 0) {
		return fw.RunRequest{}, &CheckError{Reason: fmt.Sprintf("The agent %s has no budget, so the front does not run it to write a check: a run without a limit would be a silent cost. Give the agent a budget, with at least one of maxCostUSD, maxTurns, maxTokens, and maxToolCalls, and ask again.", d.Agent)}
	}
	budget := *b
	if budget.Deadline <= 0 || budget.Deadline > s.writeWait {
		budget.Deadline = s.writeWait
	}
	principal, kind := "", ""
	if p := fw.PrincipalFrom(ctx); p != nil {
		principal, kind = p.Subject, p.Kind
	}
	return fw.RunRequest{
		ID:           fw.NewID(),
		Agent:        d.Agent,
		Input:        "Write the check of the duty " + d.Name + " for these words of a person:\n\n" + words,
		Instructions: s.writeInstructions(prof),
		Budget:       &budget,
		Metadata: map[string]string{
			fleet.MetaInternal: PurposeWriteCheck + ":" + d.Name,
			fw.MetaTitle:       "Write the check of the duty " + d.Name,
		},
		Principal:     principal,
		PrincipalKind: kind,
	}, nil
}

// writeInstructions are the fixed instructions of the run that writes a
// check: the record checkFirst of the contract of the duties, the looks
// and the secrets that this front offers now, and the tools of the agent.
func (s *Service) writeInstructions(prof fw.AgentProfile) string {
	_, _, looks := Offered()
	var b strings.Builder
	b.WriteString(`You write the check of a duty. A duty is work that you do on your own, on a schedule. A check looks for work before the duty wakes you. It calls one tool or reads one HTTP address, finds a list of items in the answer, and wakes you only when the items hold work. The check itself calls no model.

The person said in words what the check looks for. Turn the words into the record checkFirst. Your whole answer is that one JSON object, with nothing before it and nothing after it.

The record:

{
  "look": {"tool": "<the name of one of your tools>", "input": {<the input of the tool>}},
  "items": "<the dotted path of the list of items in the answer, such as result.records; leave it out when the answer is the list>",
  "key": "<the dotted path of what makes an item new, such as number>",
  "where": [{"field": "<a dotted path in an item>", "oneOf": [1, 2]}],
  "wake": {"when": "new"}
}

Rules of the record:
- look holds exactly one of tool and http. tool takes input, a JSON object. http is {"url": "https://...", "secret": "<a name of the list of secrets>", "header": "<a header name>"}, and secret and header are only for an address that needs the secret.
- items and key are dotted paths of keys, such as result.records and number.
- Each entry of where has field and exactly one of is, isNot, oneOf, contains, atLeast, and atMost. Put a filter that the tool or the address can do in the input or in the query of the address. Put the other filters in where.
- wake.when is new, any, or atLeast. new, the default, wakes you only for items whose key no earlier check saw, and it needs key. any wakes you when the check finds at least one item. atLeast takes count, such as {"when": "atLeast", "count": 5}.
- Use no other field.
`)
	fmt.Fprintf(&b, "\nThe looks that this front offers now: %s.\n", strings.Join(looks, ", "))
	if len(s.cfg.Secrets) == 0 {
		b.WriteString("The secrets that a check can send: none. An http look names no secret.\n")
	} else {
		b.WriteString("The secrets that a check can send, each only to its hosts:\n")
		for _, r := range s.cfg.Secrets {
			hosts := "no host"
			if len(r.Hosts) > 0 {
				hosts = strings.Join(r.Hosts, ", ")
			}
			fmt.Fprintf(&b, "- %s: %s\n", r.Name, hosts)
		}
	}
	if len(prof.Tools) > 0 {
		fmt.Fprintf(&b, "Your profile names these tools: %s. Pick a tool that only reads, such as a list or a search.\n", strings.Join(prof.Tools, ", "))
	} else {
		b.WriteString("Pick one of your tools that only reads, such as a list or a search.\n")
	}
	b.WriteString(`
You can call a tool that only reads once, to see the shape of its answer. Do not call a tool that changes anything. Then answer with the JSON object alone.`)
	return b.String()
}

// parseCheck reads the answer of the agent strictly: one JSON object of
// the record checkFirst, with no field that the record does not have and
// nothing after it. A fence of Markdown around the object comes off.
func parseCheck(answer string) (*CheckFirst, error) {
	text := strings.TrimSpace(answer)
	if strings.HasPrefix(text, "```") {
		nl := strings.IndexByte(text, '\n')
		if nl < 0 || !strings.HasSuffix(text, "```") || len(text) < nl+4 {
			return nil, errors.New("the fence around the answer does not close")
		}
		text = strings.TrimSpace(text[nl+1 : len(text)-3])
	}
	if !strings.HasPrefix(text, "{") {
		return nil, errors.New("the answer does not start with a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var c CheckFirst
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	var more json.RawMessage
	if err := dec.Decode(&more); err != io.EOF {
		return nil, errors.New("the answer holds more than the JSON object")
	}
	return &c, nil
}

// orWords returns s, or alt when s is empty.
func orWords(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}
