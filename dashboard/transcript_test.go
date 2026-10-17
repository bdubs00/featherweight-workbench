package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestTranscript(t *testing.T) {
	seq := uint64(0)
	ev := func(typ string, data any) fw.Event {
		seq++
		e := fw.MustEvent("r1", typ, data)
		e.Seq = seq
		return e
	}
	events := []fw.Event{
		ev(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "Review PR 42", Principal: "alice"}),
		ev(fw.EvRunStarted, fw.RunStartedData{Model: "fake"}),
		ev(fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{
			{Type: fw.BlockOpaque, Provider: "anthropic"},
			fw.Text("Let me look."),
			{Type: fw.BlockToolUse, ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"git push"}`)},
		}}),
		ev(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "bash", CallID: "c1", Input: json.RawMessage(`{"command":"git push"}`), Reason: "rule: ask destructive"}),
		ev(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "allow", Decider: "bob", Note: "fine"}),
		ev(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c1", Tool: "bash", Content: []fw.Block{fw.Text("pushed")}}),
		ev(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleSystem, Note: fw.NoteTime, Content: []fw.Block{fw.Text("It is 9:00.")}}),
		ev(fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text("Done.")}}),
		ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "Done."}),
		ev(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("Thanks")}}),
		ev(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "model down"}),
	}
	items := Transcript(events)
	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	want := "user assistant tool ask decided result assistant end user end"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds = %s\nwant    %s", got, want)
	}
	if it := items[0]; it.Text != "Review PR 42" || it.Who != "alice" || it.Seq != 1 {
		t.Errorf("user = %+v", it)
	}
	if it := items[2]; it.Tool != "bash" || it.CallID != "c1" || string(it.Input) != `{"command":"git push"}` || it.Seq != 3 {
		t.Errorf("tool = %+v", it)
	}
	if it := items[3]; it.ID != "ap1" || it.Detail != "rule: ask destructive" {
		t.Errorf("ask = %+v", it)
	}
	if it := items[4]; it.Text != "allow" || it.Who != "bob" || it.Detail != "fine" {
		t.Errorf("decided = %+v", it)
	}
	if it := items[5]; it.CallID != "c1" || it.Text != "pushed" || it.IsError {
		t.Errorf("result = %+v", it)
	}
	if it := items[9]; it.Text != fw.OutcomeFailed || !it.IsError || it.Detail != "model down" {
		t.Errorf("end = %+v", it)
	}
}

// The files of a user message ride on its item. Their file events make
// no item, because the message shows them.
func TestTranscriptAttachments(t *testing.T) {
	ref := func(p string) fw.Block {
		return fw.Block{Type: fw.BlockFile, File: &fw.FileRef{Path: p, Name: p[len("attachments/"):], MediaType: "image/png", Size: 3}}
	}
	items := Transcript([]fw.Event{
		fw.MustEvent("r1", fw.EvRunQueued, fw.RunQueuedData{Input: "look", Files: []fw.Block{ref("attachments/a.png")}}),
		fw.MustEvent("r1", fw.EvFileChanged, fw.FileChangedData{Op: fw.FileCreated, Path: "attachments/a.png", Size: 3}),
		fw.MustEvent("r1", fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{ref("attachments/b.png")}}),
	})
	if len(items) != 2 || items[0].Text != "look" || len(items[0].Files) != 1 || items[0].Files[0].Path != "attachments/a.png" {
		t.Fatalf("items = %+v", items)
	}
	if it := items[1]; it.Kind != ItemUser || it.Text != "" || len(it.Files) != 1 || it.Files[0].Name != "b.png" {
		t.Fatalf("file-only message = %+v", it)
	}
}

func TestTranscriptCutsLongValues(t *testing.T) {
	long := strings.Repeat("é", MaxItemText) // two bytes each
	input, _ := json.Marshal(map[string]string{"content": long})
	items := Transcript([]fw.Event{
		fw.MustEvent("r1", fw.EvRunQueued, fw.RunQueuedData{Input: long}),
		fw.MustEvent("r1", fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{{Type: fw.BlockToolUse, ID: "c", Name: "files", Input: input}}}),
	})
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if it := items[0]; !it.Cut || len(it.Text) > MaxItemText || !strings.HasPrefix(long, it.Text) {
		t.Errorf("text cut = %v, %d bytes", it.Cut, len(it.Text))
	}
	var s string
	if it := items[1]; !it.Cut || json.Unmarshal(it.Input, &s) != nil || len(s) > MaxItemText {
		t.Errorf("input cut = %v, %q", it.Cut, string(it.Input[:20]))
	}
}

func TestTranscriptExtraTools(t *testing.T) {
	e := fw.MustEvent("r1", fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "go", ExtraTools: []string{"bash", "http"}})
	items := Transcript([]fw.Event{e})
	if len(items) != 2 || items[0].Kind != ItemUser || items[1].Kind != ItemTools || items[1].Text != "bash,http" {
		t.Fatalf("items = %+v", items)
	}
}
