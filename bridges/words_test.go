package bridges

// This test guards the words of the system lines of a channel. The page
// says channel, agent, and job, so no system line says bridge, seat,
// worker, duty, team, or fired. The test reads the source of the package with
// go/ast, so a new line that uses an old word fails here before it reaches
// a person.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// oldWords matches the words that a person no longer reads in a channel:
// a member leaves a channel, and nobody fires it.
var oldWords = regexp.MustCompile(`(?i)\b(bridges?|seats?|seated|seating|workers?|duty|duties|teams?|fired)\b`)

// notWords names the functions whose texts are no words, such as the path
// of a page of the wiki, so the test does not read them.
var notWords = map[string]bool{"reportPath": true}

func TestSystemLinesSayChannelAndAgent(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	funcs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	lines := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "system" {
					return true
				}
				lines++
				for _, text := range lineTexts(call.Args[1], fd.Body, funcs, map[string]bool{}) {
					if w := oldWords.FindString(text); w != "" {
						t.Errorf("%s: a system line says %q: %q", fset.Position(call.Pos()), w, text)
					}
				}
				return true
			})
		}
	}
	if lines < 20 {
		t.Fatalf("the test found %d calls of system, and the package has more", lines)
	}
}

// lineTexts returns the string literals that can reach the text of a
// system line from e: the literals themselves, the parts of a sum, the
// arguments of fmt.Sprintf, the literals of a function of the package that
// e calls, and what the function body assigns to a variable that e names.
// A call of a method or of another package, such as err.Error or
// strings.TrimPrefix, gives no words of the server.
func lineTexts(e ast.Expr, body *ast.BlockStmt, funcs map[string]*ast.FuncDecl, seen map[string]bool) []string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return []string{s}
			}
		}
	case *ast.ParenExpr:
		return lineTexts(x.X, body, funcs, seen)
	case *ast.BinaryExpr:
		return append(lineTexts(x.X, body, funcs, seen), lineTexts(x.Y, body, funcs, seen)...)
	case *ast.CallExpr:
		switch fn := x.Fun.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "fmt" && fn.Sel.Name == "Sprintf" {
				var out []string
				for _, a := range x.Args {
					out = append(out, lineTexts(a, body, funcs, seen)...)
				}
				return out
			}
		case *ast.Ident:
			fd := funcs[fn.Name]
			if fd == nil || notWords[fn.Name] || seen[fn.Name] {
				return nil
			}
			seen[fn.Name] = true
			var out []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if ret, ok := n.(*ast.ReturnStmt); ok {
					for _, r := range ret.Results {
						out = append(out, lineTexts(r, fd.Body, funcs, seen)...)
					}
				}
				return true
			})
			for _, a := range x.Args {
				out = append(out, lineTexts(a, body, funcs, seen)...)
			}
			return out
		}
	case *ast.Ident:
		key := "var " + x.Name
		if seen[key] {
			return nil
		}
		seen[key] = true
		var out []string
		ast.Inspect(body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == x.Name && i < len(as.Rhs) {
					out = append(out, lineTexts(as.Rhs[i], body, funcs, seen)...)
				}
			}
			return true
		})
		return out
	}
	return nil
}
