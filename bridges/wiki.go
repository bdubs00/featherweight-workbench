package bridges

// This file finds the pages of the wiki that belong to the work of a
// bridge (docs/ARCHITECTURE.md section 18.10). To a person, the pages that
// the runs of a bridge wrote are the work of the bridge, wherever the
// agents put them, such as k3s/monitoring/alerts.md. Each version of a
// page records the run that wrote it (pages.PageInfo.Run), and the front
// knows the runs of each bridge, so the list needs no new record: it is
// the list of the wiki, which each replica keeps by revision, filtered by
// the runs of the bridge.

import (
	"context"
	"strings"

	"github.com/bdubs00/featherweight-workbench/pages"
)

// BridgePage is a page of the wiki that belongs to the work of a bridge:
// the fields of the list of the wiki, and the seat whose run wrote its
// newest version, when one did.
type BridgePage struct {
	pages.PageInfo
	Seat string `json:"seat,omitempty"`
}

// BridgePages is the answer of GET /v1/bridges/{id}/pages.
type BridgePages struct {
	Bridge string       `json:"bridge"`
	Pages  []BridgePage `json:"pages"`
	Count  int          `json:"count"`
}

// pagesOf returns the pages of the wiki that belong to the work of a
// bridge, sorted by path: each page under bridges/<id>/, such as the
// report, and each page whose newest version a run of the bridge wrote: a
// run of a seat, a helper of a seat, a child of the manager run of a spawn
// bridge, or that manager run. The runs are the runs of the batch of the
// bridge, in this process and in the view, and the runs that the seats
// list, which the view can forget. A page that a run of the bridge wrote
// and somebody wrote again since belongs to its newest writer. A process
// without a wiki has no pages.
func (s *Store) pagesOf(ctx context.Context, rec *record) ([]BridgePage, error) {
	w := pages.Of(s.a)
	if w == nil {
		return []BridgePage{}, nil
	}
	fctx := s.asFront(ctx)
	list, err := w.List(fctx)
	if err != nil {
		return nil, err
	}
	runs, err := s.runsOf(fctx, rec.ID)
	if err != nil {
		return nil, err
	}
	ours := map[string]bool{}
	for id := range runs {
		ours[id] = true
	}
	for _, m := range rec.Members {
		for _, id := range m.Runs {
			ours[id] = true
		}
	}
	prefix := "bridges/" + rec.ID + "/"
	out := []BridgePage{}
	for _, info := range list {
		if !strings.HasPrefix(info.Path, prefix) && (info.Run == "" || !ours[info.Run]) {
			continue
		}
		bp := BridgePage{PageInfo: info}
		if info.Run != "" {
			bp.Seat = rec.seatOfRun(info.Run)
			if r := runs[info.Run]; bp.Seat == "" && r != nil {
				bp.Seat = seatOrAbove(rec, r, runs)
			}
		}
		out = append(out, bp)
	}
	return out, nil
}

// readsWiki decides whether the caller in ctx reads the wiki, by the rule
// of the wiki (pages.Allowed), because the list of the pages of a bridge
// shows pages of the wiki.
func (s *Store) readsWiki(ctx context.Context) error {
	return pages.Allowed(ctx, s.a, false)
}
