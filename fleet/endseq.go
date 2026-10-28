package fleet

// This file puts a thumb of a person on the end of a run whose owner asked
// to check its result. The page and the check of a result react to the
// run as a whole, with seq 0. The kernel records a quality receipt only
// for a thumb on the fw.run.finished of a completed run (the kernel
// architecture, section 10.1). So the front names the newest
// fw.run.finished of the run before the reaction goes to the kernel, and
// the thumb also counts as a receipt. A thumb down with a note on such a
// run is Needs work: the front also sends the note to the agent as a
// follow-up message, so the run works again.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// endEvents is how many of the last events of a run of a member the front
// reads to find its end.
const endEvents = 50

// FinishedSeq returns the sequence number of the newest fw.run.finished
// of the run id: from the ledger of this process when member is "", else
// from the last events of the member, with the credential of ctx. It
// returns 0 when it finds none.
func FinishedSeq(ctx context.Context, a *fw.Agent, member, id string) uint64 {
	var end uint64
	if member == "" {
		for e, err := range a.Ledger().Read(ctx, id, 0) {
			if err != nil {
				return end
			}
			if e.Type == fw.EvRunFinished {
				end = e.Seq
			}
		}
		return end
	}
	v := Of(a)
	if v == nil {
		return 0
	}
	_, events, _, err := MemberEvents(ctx, v, member, id, 0, endEvents)
	if err != nil {
		return 0
	}
	for _, e := range events {
		if e.Type == fw.EvRunFinished && e.Seq > end {
			end = e.Seq
		}
	}
	return end
}

// AtEnd returns the body of a reaction with seq set to the end of the run
// that end finds, when the body reacts to the run as a whole (seq 0 or no
// seq) with a thumb up or down. A reaction that a person takes back with
// seq 0 goes to the end too, where the thumb is. A heart, a reaction on an
// event, a body that does not read, and a run without an end leave the
// body as it is. end runs only when it is needed.
func AtEnd(body []byte, end func() uint64) []byte {
	d, ok := thumbOf(body)
	if !ok || (d.Seq != nil && *d.Seq != 0) {
		return body
	}
	seq := end()
	if seq == 0 {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	for k := range m {
		if strings.EqualFold(k, "seq") {
			delete(m, k)
		}
	}
	raw, _ := json.Marshal(seq)
	m["seq"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// thumb is the body of a reaction, as the kernel reads it.
type thumb struct {
	Seq      *uint64 `json:"seq"`
	Reaction string  `json:"reaction"`
	Note     string  `json:"note"`
	By       string  `json:"by"`
	Remove   bool    `json:"remove"`
}

// thumbOf reads the body of a thumb up or down, and reports false for
// another reaction or a body that does not read.
func thumbOf(body []byte) (thumb, bool) {
	var d thumb
	if json.Unmarshal(body, &d) != nil {
		return d, false
	}
	d.Reaction = strings.ToLower(strings.TrimSpace(d.Reaction))
	return d, d.Reaction == fw.ReactionUp || d.Reaction == fw.ReactionDown
}

// sendsBack reports whether a body is Needs work: a thumb down with a note
// that a person gives, not takes back.
func sendsBack(body []byte) (string, bool) {
	d, ok := thumbOf(body)
	note := strings.TrimSpace(d.Note)
	return note, ok && !d.Remove && d.Reaction == fw.ReactionDown && note != ""
}

// toCheck reports whether the owner of a run asked to check its result.
// Only the reactions on such a run go on its end, so the thumbs of a chat
// stay where they were.
func toCheck(r *fw.Run) bool { return r != nil && r.Metadata[MetaReview] == "1" }

// localCheck answers a reaction on a run of this process whose owner asked
// to check its result (section 26.4 of the architecture): the thumb goes
// on the end of the run, and Needs work sends the note to the agent as a
// follow-up. It reports false when the kernel answers the request: for
// another run, and for a body that names a person, which the kernel
// decides.
func (rl *Relay) localCheck(w http.ResponseWriter, r *http.Request, id string) bool {
	ctx := r.Context()
	run, err := rl.a.Get(ctx, id)
	if err != nil || !toCheck(run) {
		return false
	}
	body, ok := readBody(w, r)
	if !ok {
		return true
	}
	body = AtEnd(body, func() uint64 { return FinishedSeq(ctx, rl.a, "", id) })
	d, thumbs := thumbOf(body)
	if !thumbs || d.By != "" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return false
	}
	rd := fw.ReactionData{Reaction: d.Reaction, Note: d.Note, Removed: d.Remove}
	if d.Seq != nil {
		rd.Seq = *d.Seq
	}
	if err := rl.a.React(ctx, id, rd); err != nil {
		fail(w, r, err)
		return true
	}
	if note, back := sendsBack(body); back {
		if err := rl.a.Send(ctx, id, fw.Message{Role: fw.RoleUser, Content: []fw.Block{fw.Text(SendBackText(note))}}, fw.OriginHTTP); err != nil {
			fail(w, r, err)
			return true
		}
	}
	after, err := rl.a.Get(ctx, id)
	if err != nil {
		fail(w, r, err)
		return true
	}
	native.WriteJSON(w, http.StatusOK, after)
	return true
}

// remoteCheck answers a reaction on a run of a member whose owner asked to
// check its result, with the credential and the person of ctx: the thumb
// goes on the end of the run, and Needs work sends the note to the agent
// as a follow-up after the member took the thumb.
func (rl *Relay) remoteCheck(ctx context.Context, w http.ResponseWriter, r *http.Request, run *fw.Run, body []byte) {
	v := Of(rl.a)
	body = AtEnd(body, func() uint64 { return FinishedSeq(ctx, rl.a, run.Member, run.ID) })
	path := "/v1/runs/" + url.PathEscape(run.ID)
	var out json.RawMessage
	if err := PostJSON(ctx, v, run.Member, path+"/reactions", json.RawMessage(body), &out); err != nil {
		fail(w, r, err)
		return
	}
	if note, back := sendsBack(body); back {
		if err := PostJSON(ctx, v, run.Member, path+"/messages", map[string]string{"content": SendBackText(note)}, nil); err != nil {
			fail(w, r, err)
			return
		}
	}
	if len(out) == 0 {
		out = json.RawMessage(`{}`)
	}
	native.WriteJSON(w, http.StatusOK, out)
}
