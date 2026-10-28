package main

// This file is the test of parity of the workbench (docs/ARCHITECTURE.md
// section 24.5): each route of the native surface that changes something
// needs an MCP tool that calls the same Go function, or an exception with
// a reason. The test reads the source of the workbench with go/ast, so it
// needs no cluster and no service: the routes are the patterns that the
// packages mount with Handle and HandlePublic of the kit of the native
// surface, or list in a table of routes, and the tools are the names of
// the sdk.Tool literals of the MCP server.

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// parity is the MCP tool of a route that changes something, or the
// reason why no tool calls its Go function.
type parity struct{ tool, why string }

// parityRoutes holds every route of the workbench that changes something.
// A new route needs an entry: a tool that calls its Go function, or the
// reason why it has none.
var parityRoutes = map[string]parity{
	// The agents, the builder, the approvals, and the connections
	// (surface/web).
	"PUT /v1/agents/{name}":                          {tool: "agent_put"},
	"DELETE /v1/agents/{name}":                       {tool: "agent_delete"},
	"POST /v1/builder":                               {tool: "agent_builder"},
	"POST /v1/runs/{id}/approvals/{approval}/rule":   {tool: "approve"},
	"POST /v1/connections/{server}":                  {why: "it starts a sign-in at the service in the browser of the person; the tool connections gives the address that starts it"},
	"DELETE /v1/connections/{server}":                {tool: "disconnect"},
	"POST /ui/refresh":                               {why: "the page renews the token of a browser sign-in with the refresh cookie of the browser; an MCP client renews its own token at its issuer"},
	"DELETE /ui/refresh":                             {why: "the page forgets the refresh cookie of the browser at a sign-out; an MCP client holds no such cookie"},
	"POST /v1/tokens":                                {why: "the broker of the tokens of a connection: a worker calls it for a run with the static token, and no person or agent does"},
	"POST /v1/tokens/reject":                         {why: "the broker of the tokens of a connection: a worker calls it for a run with the static token, and no person or agent does"},
	"POST /v1/tokens/forget":                         {why: "the broker of the tokens of a connection: a worker calls it for a run with the static token, and no person or agent does"},
	"POST /v1/sources/{name}":                        {why: "another app calls it with the credential of the webhook of the source"},
	"PATCH /v1/duties":                               {tool: "duties_pause_all"},
	"PUT /v1/duties/{name}":                          {tool: "duty_put"},
	"PATCH /v1/duties/{name}":                        {tool: "duty_pause"},
	"DELETE /v1/duties/{name}":                       {tool: "duty_delete"},
	"POST /v1/duties/{name}/run":                     {tool: "duty_run_now"},
	"POST /v1/duties/{name}/write-check":             {tool: "duty_write_check"},
	"POST /v1/duties/{name}/call":                    {why: "another app calls it with the credential of the webhook of the duty; a person or an agent uses duty_run_now"},
	"PUT /v1/notifications":                          {tool: "notifications_put"},
	"POST /v1/notifications/test":                    {tool: "notification_test"},
	"PUT /v1/playbooks/{name}":                       {tool: "playbook_put"},
	"DELETE /v1/playbooks/{name}":                    {tool: "playbook_delete"},
	"POST /v1/bridges":                               {tool: "bridge_open"},
	"PATCH /v1/bridges/{id}":                         {tool: "bridge_patch"},
	"PUT /v1/bridges/{id}/read":                      {tool: "bridge_mark_read"},
	"POST /v1/bridges/{id}/messages":                 {tool: "bridge_post"},
	"POST /v1/bridges/{id}/messages/{seq}/reactions": {tool: "bridge_react"},
	"POST /v1/bridges/{id}/questions/{seq}/settle":   {tool: "bridge_settle_question"},
	"PATCH /v1/conversations/{run}":                  {tool: "conversation_patch"},
	"PUT /v1/value/estimates":                        {tool: "value_estimate"},
	"POST /v1/work/plan":                             {tool: "work_plan"},
	"POST /v1/work/review":                           {tool: "work_review"},
	"POST /v1/work/letgo":                            {tool: "work_letgo"},
	"POST /v1/bridges/{id}/members":                  {tool: "bridge_add_member"},
	"DELETE /v1/bridges/{id}/members/{handle}":       {tool: "bridge_remove_member"},
	"PATCH /v1/bridges/{id}/members/{handle}":        {tool: "bridge_member_patch"},
	"POST /v1/pools":                                 {tool: "pool_put"},
	"PATCH /v1/pools/{name}":                         {tool: "pool_put"},
	"DELETE /v1/pools/{name}":                        {tool: "pool_delete"},
	"GET /v1/pools/{name}/packages":                  {tool: "packages_get"},
	"PUT /v1/pools/{name}/packages":                  {tool: "packages_put"},
	"DELETE /v1/fleet/orphans":                       {tool: "orphans_remove"},
	"PUT /v1/routines/{agent}/{name}":                {tool: "routine_put"},
	"POST /v1/routines/{agent}/{name}/standing":      {tool: "routine_standing"},
	"PUT /v1/pages/{path...}":                        {tool: "pages_write"},
	"DELETE /v1/pages/{path...}":                     {why: "no tool yet: an agent keeps the wiki with pages_write, and a removal is for a person on the page; a later build adds pages_delete"},
	"PUT /v1/faces/{agent}":                          {tool: "faces_put"},
	"DELETE /v1/faces/{agent}":                       {why: "no tool yet: a person gives an agent its identicon back on the page; a later build adds the removal to faces_put"},
	"PATCH /v1/faces/{agent}":                        {why: "no tool yet: a person sets the display name and the tagline of an agent on the page; a later build adds them to faces_put"},
	"POST /v1/bossmode/runs/{id}/skill":              {tool: "skill_send"},
	"POST /v1/bossmode/skills/preview":               {tool: "skill_preview"},
	"POST /v1/bossmode/skills":                       {tool: "skill_install"},
	"DELETE /v1/bossmode/skills/{name}":              {why: "no tool yet: an admin removes an imported skill on the page; a later build adds skill_remove"},
}

// unreadable names the files whose calls of Handle or HandlePublic take a
// pattern that the scan does not read as a constant, with the reason. The
// scan reads the patterns of such a file from its table of routes.
var unreadable = map[string]string{
	"duties/routes.go":   "mount mounts the routes of the table, which the scan reads",
	"notify/routes.go":   "mount mounts the routes of the table, which the scan reads",
	"surface/web/web.go": "mount mounts the routes of its table, which the scan reads",
	"surface/mcp/mcp.go": "the endpoint of the MCP server on a mux of its own, which is no route of the native API",
}

// patternRE is a route pattern of net/http with a method.
var patternRE = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE) /\S*$`)

// Each route of the workbench that changes something has an MCP tool that
// calls its Go function, or an exception with a reason.
func TestParity(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := newScanner(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.scan(); err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"PUT /v1/duties/{name}", "POST /v1/sources/{name}", "PUT /v1/agents/{name}", "POST /v1/tokens"} {
		if !sc.routes[must] {
			t.Fatalf("the scan found no route %s; it found %v", must, sortedKeys(sc.routes))
		}
	}
	for _, name := range []string{"duty_put", "run", "bridge_post", "dashboard"} {
		if !sc.tools[name] {
			t.Fatalf("the scan found no tool %s; it found %v", name, sortedKeys(sc.tools))
		}
	}
	for _, route := range sortedKeys(sc.routes) {
		if strings.HasPrefix(route, "GET ") || strings.HasPrefix(route, "HEAD ") {
			continue
		}
		p, ok := parityRoutes[route]
		switch {
		case !ok:
			t.Errorf("the route %s changes something and has no MCP tool: add a tool that calls its Go function, or an entry in parityRoutes with the reason", route)
		case (p.tool == "") == (p.why == ""):
			t.Errorf("the entry of %s needs a tool or a reason, not both", route)
		case p.tool != "" && !sc.tools[p.tool]:
			t.Errorf("the route %s names the tool %s, which no package of the workbench declares", route, p.tool)
		}
	}
	for _, route := range sortedKeys(parityRoutes) {
		if !sc.routes[route] {
			t.Errorf("parityRoutes names %s, which no package of the workbench mounts", route)
		}
	}
	for _, file := range sortedKeys(sc.unread) {
		if _, ok := unreadable[file]; !ok {
			t.Errorf("%s mounts a route whose pattern the scan does not read (%s): write the pattern as a constant, or name the file in unreadable with the reason", file, sc.unread[file])
		}
	}
	for _, file := range sortedKeys(unreadable) {
		if _, ok := sc.unread[file]; !ok {
			t.Errorf("unreadable names %s, which mounts no route with a pattern that the scan does not read", file)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// scanner reads the routes and the tools of the workbench from its source.
// It resolves a constant of a package of the workbench or of the kernel,
// so a pattern such as "POST "+tokens.PathToken reads.
type scanner struct {
	root    string
	modules map[string]string // module path to directory
	fset    *token.FileSet
	pkgs    map[string]*pkg // import path to package

	routes map[string]bool   // the route patterns
	tools  map[string]bool   // the tool names
	unread map[string]string // file to the position of a pattern that does not read
}

// pkg is a parsed package: its files and its constants.
type pkg struct {
	path   string
	files  []*file
	consts map[string]constant
}

// file is a parsed file with the import names of its package.
type file struct {
	rel     string
	ast     *ast.File
	imports map[string]string // name to import path
	pkg     *pkg
}

// constant is the expression of a constant and the file that holds it.
type constant struct {
	expr ast.Expr
	f    *file
}

const workbenchModule = "github.com/bdubs00/featherweight-workbench"

// newScanner returns a scanner of the workbench at root, with the kernel
// at the directory that the replace directive of go.mod names.
func newScanner(root string) (*scanner, error) {
	sc := &scanner{root: root, modules: map[string]string{workbenchModule: root}, fset: token.NewFileSet(), pkgs: map[string]*pkg{},
		routes: map[string]bool{}, tools: map[string]bool{}, unread: map[string]string{}}
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) == 4 && fields[0] == "replace" && fields[2] == "=>" {
			sc.modules[fields[1]] = filepath.Join(root, fields[3])
		}
	}
	return sc, lines.Err()
}

// dirOf returns the directory of an import path of a module that the
// scanner reads, or "".
func (sc *scanner) dirOf(path string) string {
	for mod, dir := range sc.modules {
		if path == mod {
			return dir
		}
		if rest, ok := strings.CutPrefix(path, mod+"/"); ok {
			return filepath.Join(dir, filepath.FromSlash(rest))
		}
	}
	return ""
}

// load parses the package of an import path once, without its tests.
func (sc *scanner) load(path string) (*pkg, error) {
	if p, ok := sc.pkgs[path]; ok {
		return p, nil
	}
	dir := sc.dirOf(path)
	p := &pkg{path: path, consts: map[string]constant{}}
	sc.pkgs[path] = p
	if dir == "" {
		return p, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		full := filepath.Join(dir, name)
		af, err := parser.ParseFile(sc.fset, full, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(sc.root, full)
		f := &file{rel: filepath.ToSlash(rel), ast: af, imports: map[string]string{}, pkg: p}
		for _, im := range af.Imports {
			ipath, _ := strconv.Unquote(im.Path.Value)
			name := ipath[strings.LastIndex(ipath, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			f.imports[name] = ipath
		}
		for _, d := range af.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						p.consts[n.Name] = constant{expr: vs.Values[i], f: f}
					}
				}
			}
		}
		p.files = append(p.files, f)
	}
	return p, nil
}

// resolve returns the value of a string expression of a file: a literal,
// a constant of its package or of an imported package of the workbench or
// of the kernel, and a sum of those.
func (sc *scanner) resolve(e ast.Expr, f *file, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return sc.resolve(x.X, f, depth+1)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok1 := sc.resolve(x.X, f, depth+1)
		b, ok2 := sc.resolve(x.Y, f, depth+1)
		return a + b, ok1 && ok2
	case *ast.Ident:
		if c, ok := f.pkg.consts[x.Name]; ok {
			return sc.resolve(c.expr, c.f, depth+1)
		}
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		path, ok := f.imports[id.Name]
		if !ok {
			return "", false
		}
		p, err := sc.load(path)
		if err != nil {
			return "", false
		}
		if c, ok := p.consts[x.Sel.Name]; ok {
			return sc.resolve(c.expr, c.f, depth+1)
		}
	}
	return "", false
}

// scan reads every package of the workbench.
func (sc *scanner) scan() error {
	return filepath.WalkDir(sc.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != sc.root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(sc.root, path)
		ipath := workbenchModule
		if rel != "." {
			ipath += "/" + filepath.ToSlash(rel)
		}
		p, err := sc.load(ipath)
		if err != nil {
			return err
		}
		for _, f := range p.files {
			sc.scanFile(f)
		}
		return nil
	})
}

// scanFile reads the routes and the tools of one file.
func (sc *scanner) scanFile(f *file) {
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandlePublic") || len(x.Args) == 0 {
				return true
			}
			if s, ok := sc.resolve(x.Args[0], f, 0); ok && patternRE.MatchString(s) {
				sc.routes[s] = true
			} else if !ok {
				sc.unread[f.rel] = sc.fset.Position(x.Pos()).String()
			}
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Tool" {
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Name" {
							if s, ok := sc.resolve(kv.Value, f, 0); ok {
								sc.tools[s] = true
							}
						}
					}
				}
			}
			for i, el := range x.Elts {
				var v ast.Expr
				switch y := el.(type) {
				case *ast.KeyValueExpr:
					if k, ok := y.Key.(*ast.Ident); ok && k.Name == "pattern" {
						v = y.Value
					}
				default:
					if i == 0 && x.Type == nil {
						v = y
					}
				}
				if v == nil {
					continue
				}
				if s, ok := sc.resolve(v, f, 0); ok && patternRE.MatchString(s) {
					sc.routes[s] = true
				}
			}
		}
		return true
	})
}

// The scanner reads a pattern of a constant of the kernel, a table of
// routes, and the name of a tool from a constant.
func TestScanner(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := newScanner(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.scan(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"POST /v1/tokens/reject", "POST /v1/duties/{name}/call", "DELETE /v1/connections/{server}", "GET /v1/duties/options"} {
		if !sc.routes[want] {
			t.Errorf("the scan lacks the route %s", want)
		}
	}
	for _, want := range []string{"duty_write_check", "orphans_remove", "routine_put", "bridge_mark_read", "playbooks_list"} {
		if !sc.tools[want] {
			t.Errorf("the scan lacks the tool %s", want)
		}
	}
	if len(sc.unread) != len(unreadable) {
		t.Errorf("unread = %v", sc.unread)
	}
}
