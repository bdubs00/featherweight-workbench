package bridges

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-workbench/fleet"
)

// playbookCall is the input of a playbook call with check false, a
// playbook, and a files map of role files, about 8 KB, as the seat
// net-auditor sent it on the homelab.
func playbookCall(t *testing.T) json.RawMessage {
	t.Helper()
	tasks := strings.Repeat("- name: harden sshd\n  ansible.builtin.lineinfile:\n    path: /etc/ssh/sshd_config\n    line: PermitRootLogin no\n", 32)
	in := map[string]any{
		"hosts":    "lab",
		"check":    false,
		"become":   true,
		"playbook": "- hosts: lab\n  roles: [sshd, ntp, unattended-upgrades]\n" + tasks,
		"files": map[string]string{
			"roles/sshd/tasks/main.yml":                tasks,
			"roles/ntp/tasks/main.yml":                 "- name: ntp\n  ansible.builtin.apt: {name: chrony}\n",
			"roles/unattended-upgrades/tasks/main.yml": "- name: upgrades\n  ansible.builtin.apt: {name: unattended-upgrades}\n",
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 6000 {
		t.Fatalf("the call holds %d bytes, want a large one", len(b))
	}
	return b
}

// A card of a call whose input the view holds cut or not at all carries
// the whole call, which the watch reads from the owner of the run as GET
// /v1/runs/{id} reads it: the hosts, check, become, the playbook, and
// every role file. On the homelab the card of such a call said only that
// the seat wants to use ansible.playbook.
func TestCardCarriesTheWholeCall(t *testing.T) {
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	whole := playbookCall(t)
	for _, tc := range []struct {
		name string
		view json.RawMessage
	}{
		{"left out", nil},
		{"cut by the egress", json.RawMessage(`{"hosts":"lab","check":false,"playbook":"- hosts: lab [cut 4120 of 8216 bytes]"}`)},
	} {
		id := "r-" + strings.ReplaceAll(tc.name, " ", "-")
		r := workerRun(id, it.ID, "migrator-1", "migrator", fw.StateInputRequired)
		r.Pending = &fw.Approval{ID: "ap-" + id, Tool: "ansible.playbook", Input: tc.view, Reason: "rule: ask ansible.playbook: check=false"}
		owned := *r
		p := *r.Pending
		p.Input = whole
		owned.Pending = &p
		fm.own(&owned)
		// The seat holds this run now.
		if _, err := s.b.update(context.Background(), it.ID, func(rec *record) error {
			rec.member("migrator-1").Runs = append(rec.member("migrator-1").Runs, id)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		v.put(r)
		s.poll(context.Background())
		var card *Message
		for _, m := range withEvent(channel(t, s, it.ID), EventNeedsYou) {
			if m.Decision.Run == id {
				card = &m
			}
		}
		if card == nil {
			t.Fatalf("%s: no card", tc.name)
		}
		var got, want any
		if json.Unmarshal(card.Decision.Input, &got) != nil || json.Unmarshal(whole, &want) != nil || !jsonEqual(got, want) {
			t.Fatalf("%s: the card holds %s", tc.name, card.Decision.Input)
		}
		if card.Text != "migrator-1 wants to use ansible.playbook" || card.Decision.Always {
			t.Fatalf("%s: card %q, always %v", tc.name, card.Text, card.Decision.Always)
		}
		if n := len(fm.find(http.MethodGet, "/v1/runs/"+id)); n != 1 {
			t.Fatalf("%s: the owner got %d reads of the run", tc.name, n)
		}
		rec, _, _ := s.b.get(context.Background(), it.ID)
		if rec.Marks[id].Thin {
			t.Fatalf("%s: the card is marked thin", tc.name)
		}
	}
}

// A card whose first read of the whole call fails holds the input of the
// view and is marked thin. The watch reads the call again every
// wholeEvery, and once a read works it writes the card again in place,
// with the whole call, and gives the hook the changed entry.
func TestThinCardGetsTheWholeCallLater(t *testing.T) {
	h := hooked(t)
	a := newAgent(t, nil)
	s := newStore(t, a, nil)
	clock := &testClock{}
	clock.set(time.Date(2026, 10, 23, 4, 24, 0, 0, time.UTC))
	s.now = clock.now
	v, fm := withFleet(t, a)
	v.state("default/crew-0", fleet.MemberLive)
	url := serve(t, a)
	it := open(t, url)
	whole := playbookCall(t)
	r := workerRun("r-mig", it.ID, "migrator-1", "migrator", fw.StateInputRequired)
	r.Pending = &fw.Approval{ID: "ap1", Tool: "ansible.playbook", Reason: "rule: ask ansible.playbook: check=false"}
	owned := *r
	p := *r.Pending
	p.Input = whole
	owned.Pending = &p
	fm.own(&owned)
	fm.mu.Lock()
	fm.status["/v1/runs/r-mig"] = http.StatusServiceUnavailable
	fm.mu.Unlock()
	v.put(r)
	s.poll(context.Background())
	cards := withEvent(channel(t, s, it.ID), EventNeedsYou)
	if len(cards) != 1 || len(cards[0].Decision.Input) != 0 {
		t.Fatalf("the card while the owner fails = %+v", cards)
	}
	rec, _, _ := s.b.get(context.Background(), it.ID)
	if !rec.Marks["r-mig"].Thin {
		t.Fatal("the card is not marked thin")
	}
	// Within wholeEvery the watch reads nothing again.
	s.poll(context.Background())
	if n := len(fm.find(http.MethodGet, "/v1/runs/r-mig")); n != 1 {
		t.Fatalf("the owner got %d reads within wholeEvery", n)
	}
	fm.mu.Lock()
	delete(fm.status, "/v1/runs/r-mig")
	fm.mu.Unlock()
	clock.set(clock.now().Add(wholeEvery))
	s.poll(context.Background())
	msgs := channel(t, s, it.ID)
	card := msgs[cards[0].Seq-1]
	var got, want any
	if json.Unmarshal(card.Decision.Input, &got) != nil || json.Unmarshal(whole, &want) != nil || !jsonEqual(got, want) || card.Decision.State != DecisionPending {
		t.Fatalf("the card after the read = %+v", card.Decision)
	}
	if hm := h.message(card.Seq); len(hm) != 2 || len(hm[1].Decision.Input) == 0 {
		t.Fatalf("the hook got the card %d times", len(hm))
	}
	rec, _, _ = s.b.get(context.Background(), it.ID)
	if rec.Marks["r-mig"].Thin {
		t.Fatal("the card stays thin")
	}
	// A card that holds the whole call reads it no more.
	clock.set(clock.now().Add(wholeEvery))
	s.poll(context.Background())
	if n := len(fm.find(http.MethodGet, "/v1/runs/r-mig")); n != 2 {
		t.Fatalf("the owner got %d reads, want 2", n)
	}
}

// An input above the bound of a card keeps the start of each string with
// the note of a cut, and every field.
func TestFitInput(t *testing.T) {
	small := json.RawMessage(`{"a":"b"}`)
	if got := fitInput(small, 100); string(got) != string(small) {
		t.Fatalf("a small input = %s", got)
	}
	big, _ := json.Marshal(map[string]any{"check": false, "playbook": strings.Repeat("x", 50<<10), "files": map[string]string{"a.yml": strings.Repeat("y", 30<<10)}})
	got := fitInput(big, cardInputBytes)
	if len(got) > cardInputBytes || len(got) == 0 {
		t.Fatalf("the cut input holds %d bytes", len(got))
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil || v["check"] != false || !strings.Contains(v["playbook"].(string), "[cut ") || v["files"].(map[string]any)["a.yml"] == nil {
		t.Fatalf("the cut input = %.300s", got)
	}
	if !thinInput(nil) || !thinInput(json.RawMessage("null")) || !thinInput(got) || thinInput(small) {
		t.Fatal("thinInput")
	}
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
