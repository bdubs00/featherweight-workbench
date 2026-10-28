package duties

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The line of the contract of found: the first line, or the last line of
// a whole answer, in any case and with the marks of Markdown. Nothing and
// its words found nothing. An answer without the line found something.
func TestFoundLine(t *testing.T) {
	for _, c := range []struct {
		out       string
		what      string
		something bool
	}{
		{"Found: 2 hosts drift from their roles\nweb-1 and web-3.", "2 hosts drift from their roles", true},
		{"**Found:** nothing\nAll hosts match.", "nothing", false},
		{"FOUND: None.", "None", false},
		{"I looked at every host.\nFound: 0", "0", false},
		{"I looked at every host.\nFound: 0 …", "", true}, // a cut answer: the last line does not count
		{"All hosts match.", "", true},
		{"", "", true},
		{"`Found: one stale backup`", "one stale backup", true},
	} {
		what, something := FoundLine(c.out)
		if what != c.what || something != c.something {
			t.Errorf("FoundLine(%q) = %q, %v; want %q, %v", c.out, what, something, c.what, c.something)
		}
	}
}

// notify.result and notify.resultWhen normalize and check: a list of
// results takes always, a channel takes its prefix, a wrong choice is a
// problem, and found and review need a run or a check. The readback names
// who hears of a result and that the owner checks each result.
func TestResultRecord(t *testing.T) {
	d := daily("report")
	d.Notify = &Notify{Failure: []string{"owner"}, Result: []string{"owner", " channel: ops-room ", "erin@example.com"}}
	d.Review = true
	d.normalize("UTC")
	if d.Notify.ResultWhen != ResultAlways || d.Notify.Result[1] != "channel:ops-room" {
		t.Fatalf("notify = %+v", d.Notify)
	}
	if p := d.problems(); len(p) != 0 {
		t.Fatalf("problems of a good duty: %v", p)
	}
	rb := readback(&d)
	if !strings.Contains(rb, "Each result goes to the owner, the channel ops-room and erin.") || !strings.Contains(rb, reviewWords) {
		t.Fatalf("the readback = %q", rb)
	}
	d.Notify.ResultWhen = "found"
	if rb := readback(&d); !strings.Contains(rb, "A result goes to the owner, the channel ops-room and erin when the run says that it found something.") {
		t.Fatalf("the readback of found = %q", rb)
	}
	d.Notify.ResultWhen = "sometimes"
	d.Notify.Result = append(d.Notify.Result, "bad\nname")
	p := strings.Join(d.problems(), "\n")
	if !strings.Contains(p, "notify.resultWhen") || !strings.Contains(p, `notify.result: "bad\nname" is not`) {
		t.Fatalf("problems = %s", p)
	}
	post := daily("post")
	post.Do = Do{Post: &Post{Bridge: "b1", Message: "Hello."}}
	post.Limits.Budget = nil
	post.Review = true
	post.Notify = &Notify{Result: []string{"owner"}, ResultWhen: ResultFound}
	post.normalize("UTC")
	p = strings.Join(post.problems(), "\n")
	if !strings.Contains(p, "review: A duty that posts") || !strings.Contains(p, "notify.resultWhen: A duty that posts or opens a bridge has no answer") {
		t.Fatalf("problems of a post = %s", p)
	}
}

// A duty with results of found tells its agent the contract, and review
// marks each start. A completed run that found something sends its result
// to the people of notify.result; one that found nothing sends nothing,
// and neither sends a notice of another kind.
func TestResultNotice(t *testing.T) {
	f := newFront(t, "", nil)
	d := daily("drift")
	d.Notify = &Notify{Result: []string{"owner", "frank"}, ResultWhen: ResultFound}
	d.Review = true
	f.put(t, "alice", d)
	f.clock.Set(nine.Add(time.Second))
	f.pass(t)
	req := f.queue.last(t)
	if req.Metadata[MetaReview] != "1" || req.Instructions != FoundInstructions {
		t.Fatalf("the request = %+v", req)
	}
	f.view.set(req.ID, fw.StateCompleted, 0.02, func(r *fw.Run) { r.Output = "Found: nothing\nEvery host matches its role." })
	f.pass(t)
	if got := allNotices.kinds(f.a, "drift"); len(got) != 0 {
		t.Fatalf("a run that found nothing sent %v", got)
	}

	f.clock.Add(24 * time.Hour)
	f.pass(t)
	req = f.queue.last(t)
	f.view.set(req.ID, fw.StateCompleted, 0.03, func(r *fw.Run) { r.Output = "Found: web-3 drifts from its role\nThe diff is in the wiki." })
	f.pass(t)
	got := allNotices.of(f.a, "drift")
	if len(got) != 1 || got[0].Kind != NoticeResult || got[0].Run != req.ID || got[0].Reason != "Found: web-3 drifts from its role" || !slices.Equal(got[0].Who, []string{"alice", "frank"}) {
		t.Fatalf("notices = %+v", got)
	}

	// Always sends each result, with the start of the answer, and gives
	// the agent no contract.
	a := daily("always")
	a.Notify = &Notify{Result: []string{"erin"}}
	f.put(t, "alice", a)
	f.clock.Add(24 * time.Hour)
	f.pass(t)
	var ar fw.RunRequest
	for _, r := range f.queue.list() {
		if r.Metadata[MetaDuty] == "always" {
			ar = r
		}
	}
	if ar.ID == "" || ar.Instructions != "" || ar.Metadata[MetaReview] != "" {
		t.Fatalf("the request of always = %+v", ar)
	}
	f.view.set(ar.ID, fw.StateCompleted, 0.01, func(r *fw.Run) { r.Output = "Found: nothing\nNo new ticket." })
	f.pass(t)
	if got := allNotices.of(f.a, "always"); len(got) != 1 || got[0].Reason != "Found: nothing No new ticket." || !slices.Equal(got[0].Who, []string{"erin"}) {
		t.Fatalf("notices of always = %+v", got)
	}
}
