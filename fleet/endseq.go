package fleet

// This file puts a thumb of a person on the end of a run. The page and
// the check of a result react to the run as a whole, with seq 0. The
// kernel records a quality receipt only for a thumb on the fw.run.finished
// of a completed run (the kernel architecture, section 10.1). So the front
// names the newest fw.run.finished of the run before the reaction goes to
// the kernel, and the thumb also counts as a receipt.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
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
// seq) with a thumb up or down. A heart, a reaction that a person takes
// back, a reaction on an event, a body that does not read, and a run
// without an end leave the body as it is. end runs only when it is needed.
func AtEnd(body []byte, end func() uint64) []byte {
	var d struct {
		Seq      *uint64 `json:"seq"`
		Reaction string  `json:"reaction"`
		Remove   bool    `json:"remove"`
	}
	if json.Unmarshal(body, &d) != nil || d.Remove || (d.Seq != nil && *d.Seq != 0) {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(d.Reaction)) {
	case fw.ReactionUp, fw.ReactionDown:
	default:
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

// localEnd puts a thumb on the run as a whole of a run of this process on
// the end of the run, before the kernel takes the request. It reports
// false when it answered the request with a problem.
func (rl *Relay) localEnd(w http.ResponseWriter, r *http.Request, id string) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	body = AtEnd(body, func() uint64 { return FinishedSeq(r.Context(), rl.a, "", id) })
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}
