package sources

// This file finds the item of an outside system that an event names, such
// as an issue of Gitea, and hands it to Know-how (docs/KNOWHOW.md section
// 5.2). A note of an agent can name such an item as its source, with the
// version that the note read. When a later event shows the item with
// another version, the note goes out of date.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// OnItem gets the item that each event of a source names: the system, the
// key of the item in it, and its version (ItemOf). The package knowhow
// sets it at its start. Nil calls nothing. It runs on the goroutine of the
// delivery, for every event that names an item, a duplicate included, so
// it must not block.
var OnItem func(ctx context.Context, source, key, version string)

// Item is the item of an outside system that an event names. Source names
// the system, such as gitea. Key names the item in the system, and
// Version the state of the item that the event shows. A note names the
// item as item:<source>/<key>.
type Item struct {
	Source  string
	Key     string
	Version string
}

// Ref is the reference of the item in the sources of a note:
// item:<source>/<key>.
func (it Item) Ref() string { return "item:" + it.Source + "/" + it.Key }

// SourceGitea is the system of the items of Gitea.
const SourceGitea = "gitea"

// ItemOf returns the item that an event names, and false for an event
// that names none. It knows the issues and the pull requests of Gitea: a
// webhook of Gitea carries the issue or the pull request in the object
// issue or pull_request, and the API of Gitea lists an issue as the event
// itself. The key is <owner>/<repo>/issue/<number> when the event names
// its repository, else issue/<number>, because Gitea counts the issues and
// the pull requests of a repository together. The version is updated_at.
// An object of GitHub, which carries node_id, names no item.
func ItemOf(event map[string]any) (Item, bool) {
	if event == nil {
		return Item{}, false
	}
	obj := event
	for _, k := range []string{"issue", "pull_request"} {
		if m, ok := event[k].(map[string]any); ok {
			obj = m
			break
		}
	}
	if _, github := obj["node_id"]; github {
		return Item{}, false
	}
	number, ok := wholeNumber(obj["number"])
	version, _ := obj["updated_at"].(string)
	version = strings.TrimSpace(version)
	if !ok || version == "" {
		return Item{}, false
	}
	key := "issue/" + number
	repo := repositoryOf(obj)
	if repo == "" {
		repo = repositoryOf(event)
	}
	if repo != "" {
		key = repo + "/" + key
	}
	return Item{Source: SourceGitea, Key: key, Version: version}, true
}

// repositoryOf returns the full name of the repository of an object of
// Gitea, owner/repo, or "".
func repositoryOf(obj map[string]any) string {
	r, ok := obj["repository"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := r["full_name"].(string)
	name = strings.Trim(strings.TrimSpace(name), "/")
	if strings.Count(name, "/") != 1 || strings.ContainsAny(name, " \t\n") {
		return ""
	}
	return name
}

// wholeNumber reads a positive whole number of a decoded event: a
// json.Number, a float64, or a text of digits.
func wholeNumber(v any) (string, bool) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = n.String()
	case float64:
		if n != float64(int64(n)) {
			return "", false
		}
		s = fmt.Sprint(int64(n))
	case string:
		s = strings.TrimSpace(n)
	default:
		return "", false
	}
	if s == "" || s == "0" || strings.Trim(s, "0123456789") != "" {
		return "", false
	}
	return s, true
}

// itemsOf calls OnItem for each event that names an item.
func itemsOf(ctx context.Context, events []map[string]any) {
	fn := OnItem
	if fn == nil {
		return
	}
	for _, e := range events {
		if it, ok := ItemOf(e); ok {
			fn(ctx, it.Source, it.Key, it.Version)
		}
	}
}
