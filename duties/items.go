package duties

// This file hands Know-how the items of the outside systems that a duty
// sees (docs/KNOWHOW.md sections 5.2 and 6.4), such as an issue of Gitea
// in an item of a check or in the body of a call of the webhook of the
// duty. sources.ItemOf finds the item.

import (
	"context"

	"github.com/bdubs00/featherweight-workbench/sources"
)

// OnItem gets the item that an event of a duty names: each item of a
// check, and the body of a call of its webhook, as sources.ItemOf finds
// it, with the system, the key, and the version. A duplicate call counts
// too. The calls and the polls of a duty whose templates make the runs go
// through the sources and reach sources.OnItem instead. The package
// knowhow sets it at its start. Nil calls nothing. The function runs on
// the goroutine of the start, so it must not block.
var OnItem func(ctx context.Context, source, key, version string)

// itemsOf calls OnItem for each event that names an item.
func itemsOf(ctx context.Context, events []map[string]any) {
	fn := OnItem
	if fn == nil {
		return
	}
	for _, e := range events {
		if it, ok := sources.ItemOf(e); ok {
			fn(ctx, it.Source, it.Key, it.Version)
		}
	}
}
