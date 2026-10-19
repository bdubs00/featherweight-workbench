package dashboard

// This file is the chat view of a run: a projection of the ledger into the
// items that the page draws as a conversation. It reads the events only,
// so a run of another fleet member gets the same view from its events.

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Kinds of a chat item.
const (
	ItemUser      = "user"      // a message to the agent: the input or a follow-up
	ItemAssistant = "assistant" // the text of one model response
	ItemTool      = "tool"      // a tool call: Tool, CallID, Input, and Profile for propose_agent
	ItemResult    = "result"    // the result of a tool call: CallID, Text, IsError
	ItemFile      = "file"      // a change to a file: Tool, CallID, Path, Text (the operation), Detail (the old path), Size
	ItemAsk       = "ask"       // an approval request: ID, Tool, CallID, Input, Detail, and Connect for a connect
	ItemDecided   = "decided"   // a decision: ID, Text ("allow" or "deny"), Who, Detail, and Connect for a connect
	ItemNote      = "note"      // a fact about the run, such as a compaction or an error
	ItemEnd       = "end"       // the end of a turn of work: Text is the outcome
	ItemTools     = "tools"     // the tools that the run has beyond its agent: Text names them, separated by commas
)

// MaxItemText is the largest Text, Detail, or Input of an item in bytes.
// A longer value is cut, and Cut is set.
const MaxItemText = 32 << 10

// Item is one entry of the chat view of a run. Seq is the sequence number
// of the event that made it, so a page asks for the items after the last
// one it has. A result names the call that it answers with CallID, and the
// page puts it under that call.
type Item struct {
	Seq     uint64          `json:"seq"`
	Time    time.Time       `json:"time"`
	Kind    string          `json:"kind"`
	Text    string          `json:"text,omitempty"`
	Detail  string          `json:"detail,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	CallID  string          `json:"callId,omitempty"`
	ID      string          `json:"id,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Schema  json.RawMessage `json:"schema,omitempty"`
	Who     string          `json:"who,omitempty"`
	Path    string          `json:"path,omitempty"`
	Size    int64           `json:"size,omitempty"`
	IsError bool            `json:"isError,omitempty"`
	Cut     bool            `json:"cut,omitempty"`
	Files   []fw.FileRef    `json:"files,omitempty"` // the attachments of a user message
	// Profile is the profile that a propose_agent call proposes, from
	// ProposalProfile of its input.
	Profile *fw.AgentProfile `json:"profile,omitempty"`
	// Connect is set on an ask when the call waits for the person to
	// connect a service, and on its decision when the ask is in the same
	// events. An allow then means that the person connected the service,
	// and a deny that the person skipped it.
	Connect *fw.ConnectRequest `json:"connect,omitempty"`
}

// fileRefs returns the references of the file blocks.
func fileRefs(blocks []fw.Block) []fw.FileRef {
	var out []fw.FileRef
	for _, b := range blocks {
		if b.Type == fw.BlockFile && b.File != nil {
			out = append(out, *b.File)
		}
	}
	return out
}

// Transcript turns events of one run into chat items, in order. Events that
// a person does not read, such as the start of a turn or a model request,
// make no item. Thinking and other opaque provider blocks make no item.
func Transcript(events []fw.Event) []Item {
	out := make([]Item, 0, len(events))
	connects := map[string]*fw.ConnectRequest{} // the connect requests by approval id
	for _, e := range events {
		at := func(kind string) Item { return Item{Seq: e.Seq, Time: e.Time, Kind: kind} }
		switch e.Type {
		case fw.EvRunQueued:
			var d fw.RunQueuedData
			if e.Decode(&d) == nil && (d.Input != "" || len(d.Files) > 0) {
				it := at(ItemUser)
				it.Who = d.Principal
				it.setText(d.Input)
				it.Files = fileRefs(d.Files)
				out = append(out, it)
			}
			if len(d.ExtraTools) > 0 {
				it := at(ItemTools)
				it.Text = strings.Join(d.ExtraTools, ",")
				out = append(out, it)
			}
		case fw.EvMessageReceived:
			var d fw.MessageReceivedData
			if e.Decode(&d) != nil {
				continue
			}
			switch {
			case d.Role == fw.RoleUser:
				text, files := fw.TextOf(d.Content), fileRefs(d.Content)
				if text != "" || len(files) > 0 {
					it := at(ItemUser)
					it.setText(text)
					it.Files = files
					out = append(out, it)
				}
			case d.Note == fw.NoteLoop:
				it := at(ItemNote)
				it.setText("fw told the agent that it repeats itself.")
				out = append(out, it)
			}
		case fw.EvModelResponded:
			var d fw.ModelRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			if text := strings.TrimSpace(fw.TextOf(d.Content)); text != "" {
				it := at(ItemAssistant)
				it.setText(text)
				out = append(out, it)
			}
			for _, b := range d.Content {
				if b.Type != fw.BlockToolUse {
					continue
				}
				it := at(ItemTool)
				it.Tool, it.CallID = b.Name, b.ID
				it.setInput(b.Input)
				if b.Name == "propose_agent" {
					// A cut input is a JSON string, so it makes no profile.
					if p, err := ProposalProfile(it.Input); err == nil {
						it.Profile = &p
					}
				}
				out = append(out, it)
			}
		case fw.EvToolResponded:
			var d fw.ToolRespondedData
			if e.Decode(&d) != nil {
				continue
			}
			it := at(ItemResult)
			it.Tool, it.CallID, it.IsError = d.Tool, d.CallID, d.IsError
			it.setText(fw.TextOf(d.Content))
			out = append(out, it)
		case fw.EvFileChanged:
			var d fw.FileChangedData
			// A change without a tool call is an attachment, which its
			// user message shows.
			if e.Decode(&d) != nil || d.Path == "" || (d.CallID == "" && d.Tool == "") {
				continue
			}
			it := at(ItemFile)
			it.Tool, it.CallID, it.Path, it.Text, it.Detail, it.Size = d.Tool, d.CallID, d.Path, d.Op, d.OldPath, d.Size
			out = append(out, it)
		case fw.EvApprovalRequested:
			var d fw.ApprovalRequestedData
			if e.Decode(&d) != nil {
				continue
			}
			it := at(ItemAsk)
			it.ID, it.Tool, it.CallID, it.Schema, it.Connect = d.ApprovalID, d.Tool, d.CallID, d.Schema, d.Connect
			it.setInput(d.Input)
			it.setDetail(d.Reason)
			if d.Connect != nil {
				connects[d.ApprovalID] = d.Connect
				if it.Detail == "" {
					it.Detail = d.Connect.Message()
				}
			}
			out = append(out, it)
		case fw.EvApprovalDecided:
			var d fw.ApprovalDecidedData
			if e.Decode(&d) != nil {
				continue
			}
			it := at(ItemDecided)
			it.ID, it.Text, it.Who, it.Connect = d.ApprovalID, d.Decision, d.Decider, connects[d.ApprovalID]
			it.setDetail(d.Note)
			out = append(out, it)
		case fw.EvContextCompacted:
			it := at(ItemNote)
			it.Text = "Earlier messages were summarized to save space."
			out = append(out, it)
		case fw.EvBudgetExceeded:
			var d fw.BudgetExceededData
			if e.Decode(&d) == nil {
				it := at(ItemNote)
				it.IsError = true
				if d.Scope == fw.ScopeTree {
					it.setText("The run tree of run " + d.Run + " reached its limit of " + d.Limit + ", which the run shares with its child runs.")
				} else {
					it.setText("The run reached its limit of " + d.Limit + ".")
				}
				out = append(out, it)
			}
		case fw.EvLoopDetected:
			var d fw.LoopDetectedData
			if e.Decode(&d) == nil && d.Action == "failed" {
				it := at(ItemNote)
				it.IsError = true
				it.setText("The run stopped, because the agent repeated " + d.Tool + ".")
				out = append(out, it)
			}
		case fw.EvError:
			var d fw.ErrorData
			if e.Decode(&d) == nil {
				it := at(ItemNote)
				it.IsError = !d.Retryable
				it.setText(d.Message)
				out = append(out, it)
			}
		case fw.EvRunSuspended:
			it := at(ItemNote)
			it.Text = "The run paused."
			out = append(out, it)
		case fw.EvRunResumed:
			it := at(ItemNote)
			it.Text = "The run resumed."
			out = append(out, it)
		case fw.EvRunFinished:
			var d fw.RunFinishedData
			if e.Decode(&d) != nil {
				continue
			}
			it := at(ItemEnd)
			it.Text = d.Outcome
			it.IsError = d.Outcome == fw.OutcomeFailed
			it.setDetail(d.Error)
			out = append(out, it)
		}
	}
	return out
}

func (it *Item) setText(s string) {
	it.Text, it.Cut = cut(s, it.Cut)
}

func (it *Item) setDetail(s string) {
	it.Detail, it.Cut = cut(s, it.Cut)
}

// setInput keeps a JSON input up to MaxItemText. A longer input becomes a
// JSON string that holds its first part, so the item stays valid JSON.
func (it *Item) setInput(raw json.RawMessage) {
	if len(raw) <= MaxItemText {
		it.Input = raw
		return
	}
	head, _ := cut(string(raw), false)
	b, _ := json.Marshal(head)
	it.Input, it.Cut = b, true
}

// cut shortens s to MaxItemText bytes at a rune boundary.
func cut(s string, was bool) (string, bool) {
	if len(s) <= MaxItemText {
		return s, was
	}
	n := MaxItemText
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
