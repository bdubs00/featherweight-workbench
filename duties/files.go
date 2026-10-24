package duties

// This file shows the entries of workbench.sources as duties in a read
// (docs/ARCHITECTURE.md section 22.8). A source is a duty with an outside
// start that fw.yaml sets: another app calls, or an address has new items.
// A read shows it with the source file, and the routes never change it.
// Its templates stay out of the read, so a person never sees their syntax.

import (
	"fmt"
	"time"

	"github.com/bdubs00/featherweight-workbench/sources"
)

// files returns the entries of workbench.sources as duties, or none when
// the configuration names none or does not load.
func (s *Service) files() []View {
	list, err := sources.Load(s.a.Config())
	if err != nil {
		return nil
	}
	out := make([]View, 0, len(list))
	for _, src := range list {
		out = append(out, fileView(src))
	}
	return out
}

// fileOf returns the entry of workbench.sources of the name, or nil.
func (s *Service) fileOf(name string) *View {
	for _, v := range s.files() {
		if v.Name == name {
			return &v
		}
	}
	return nil
}

// fileView shows one source as a duty.
func fileView(src sources.Source) View {
	d := Duty{Name: src.Name, Agent: src.Agent, Pool: src.Pool, RunAs: src.RunAs}
	const where = " An operator sets it in the settings of the front, so it changes there."
	var readback string
	if p := src.Poll; p != nil {
		d.CheckFirst = &CheckFirst{
			Look:  Look{HTTP: &LookHTTP{URL: addressWords(p.URL), Secret: p.Secret, Header: p.Header}},
			Items: src.Items,
			Wake:  Wake{When: WakeNew},
		}
		readback = fmt.Sprintf("Every %s, it looks at %s, and each new item starts a run of %s.", durText(p.Every), addressWords(p.URL), src.Agent) + where
	} else {
		d.When.Webhook = &Webhook{Auth: WebhookAuth{Mode: src.Auth.Mode, Secret: src.Auth.Secret, Header: src.Auth.Header}}
		readback = fmt.Sprintf("When another app calls the address of this source, each new event starts a run of %s.", src.Agent) + where
	}
	return View{Duty: d, Source: SourceFile, Readback: readback, Next: []time.Time{}}
}
