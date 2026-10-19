package main

import (
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

	"github.com/baditaflorin/aivoicebus/internal/view"
)

// userFacing are the packages whose errors a person or an agent reads as they are made: the
// commands (cli, outbox, tasks), the MCP tools, the hooks, and what nodes answer (server, store,
// client). Other packages' errors reach a reader only wrapped by one of these.
var userFacing = []string{"cli", "client", "hook", "mcp", "outbox", "server", "store", "tasks"}

// errorMakers are the calls that make an error a reader sees, and which argument holds its text.
var errorMakers = map[string]int{
	"Usagef":    0, // outbox.Usagef: the command was used wrongly
	"NotFoundf": 0, // outbox.NotFoundf
	"New":       0, // errors.New
	"Errorf":    0, // fmt.Errorf
	"Error":     1, // http.Error(w, text, code): a node's refusal
	"refuse":    2, // server.refuse(w, reason, text, code)
}

// verb is a format verb, which stands for a value, not for words.
var verb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z%]`)

// errSite is one error a reader may see, as written in the source.
type errSite struct {
	pos  string
	text string // the literal parts of its text; a value made at run time shows as %v
}

// TestErrorsSayWhatToDo walks every error a person or an agent reads and fails on one that does
// not end with a next step: a command to run (in backticks, or `aivoicebus ...`), someone to ask,
// or doing it again (view.HasNextStep). An error exempt from the rule says why in a comment with
// "nextstep:" on its line or the line above: the caller that adds the step, or why none helps.
// An error that wraps another (%w, or a trailing %v) leaves the next step to what it wraps.
//
// Two kinds get their next step where they are shown, each proved by its own test: a command's
// usage error (outbox.Usagef in cli) from cli.withNextStep, which names the command's -h
// (TestUsageErrorsSayWhereFlagsAre), and what a node answers (the server's refusals, the store's
// errors) from client.StatusError, which says it in this client's words whatever the node's
// release (TestRefusalsSayWhatToDo).
func TestErrorsSayWhatToDo(t *testing.T) {
	var all, missing []errSite
	exempt, wraps, shown := 0, 0, 0
	for _, pkg := range userFacing {
		files, _ := filepath.Glob(filepath.Join("internal", pkg, "*.go"))
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, file, src, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			marked := map[int]bool{} // lines with a nextstep: comment
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if strings.Contains(c.Text, "nextstep:") {
						marked[fset.Position(c.Pos()).Line] = true
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, qual := callName(call.Fun)
				arg, ok := errorMakers[name]
				switch {
				case !ok, name == "New" && qual != "errors", name == "Errorf" && qual != "fmt", name == "Error" && qual != "http",
					name == "refuse" && qual != "", len(call.Args) <= arg:
					return true
				}
				text, literal := words(call.Args[arg])
				if !literal {
					return true // made at run time: the value it is made from is checked where it is made
				}
				line := fset.Position(call.Pos()).Line
				site := errSite{fset.Position(call.Pos()).String(), text}
				all = append(all, site)
				bare := strings.TrimSpace(verb.ReplaceAllString(text, ""))
				switch {
				case pkg == "cli" && name == "Usagef", pkg == "store", pkg == "server" && (name == "Error" || name == "refuse"):
					shown++
				case marked[line] || marked[line-1]:
					exempt++
				case strings.Contains(text, "%w") || strings.HasSuffix(strings.TrimRight(text, ".) "), "%v") || strings.Trim(bare, ":;,.() ") == "":
					wraps++
				case !view.HasNextStep(text):
					missing = append(missing, site)
				}
				return true
			})
		}
	}
	hooks := hookWarnings(t)
	for _, w := range hooks {
		all = append(all, w)
		if !view.HasNextStep(w.text) {
			missing = append(missing, w)
		}
	}
	t.Logf("%d user-facing errors in %s and %d hook warnings: %d say their next step, %d get it where shown, %d wrap another error, %d exempt, %d missing",
		len(all)-len(hooks), strings.Join(userFacing, ", "), len(hooks), len(all)-shown-wraps-exempt-len(missing), shown, wraps, exempt, len(missing))
	for _, m := range missing {
		t.Errorf("%s: no next step (a command in backticks, `aivoicebus ...`, \"ask the user ...\", or \"try again\"; or a nextstep: comment saying why not): %q", m.pos, m.text)
	}
	if len(all) < 300 {
		t.Errorf("found only %d errors: the walk is broken", len(all))
	}
}

// hookWarnings are the hook's warning lines (the warn* constants): fixed text an agent reads.
func hookWarnings(t *testing.T) []errSite {
	var out []errSite
	files, _ := filepath.Glob(filepath.Join("internal", "hook", "*.go"))
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, id := range vs.Names {
				if strings.HasPrefix(id.Name, "warn") && i < len(vs.Values) {
					if text, ok := words(vs.Values[i]); ok {
						out = append(out, errSite{fset.Position(id.Pos()).String(), text})
					}
				}
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Error("no hook warnings found: the walk is broken")
	}
	return out
}

// callName is the name a call is made by, and the package or value it is qualified with.
func callName(fun ast.Expr) (name, qual string) {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name, ""
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return f.Sel.Name, x.Name
		}
		return f.Sel.Name, "?"
	}
	return "", ""
}

// words is the text of e: its string literals joined, with %v for any part made at run time.
// literal is false when no part of it is a literal.
func words(e ast.Expr) (text string, literal bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			l, lok := words(x.X)
			r, rok := words(x.Y)
			return l + r, lok || rok
		}
	case *ast.ParenExpr:
		return words(x.X)
	case *ast.CallExpr:
		if name, qual := callName(x.Fun); name == "Sprintf" && qual == "fmt" && len(x.Args) > 0 {
			return words(x.Args[0])
		}
	}
	return "%v", false
}

// The rule itself: what counts as a next step.
func TestHasNextStep(t *testing.T) {
	yes := []string{
		"usage: claim TASK_ID",
		"no node reachable: run `aivoicebus doctor`",
		"give the full id (aivoicebus inbox --all lists ids)",
		"ask the user to update the nodes",
		"Try again in 5 minutes",
		"nothing was sent: send it again without the secret",
	}
	no := []string{
		"the conversation reached its turn limit or deadline",
		"neither a [token hand] task nor a message with a link",
		"409 Conflict: bad thing",
		"ask nothing",
	}
	for _, s := range yes {
		if !view.HasNextStep(s) {
			t.Errorf("HasNextStep(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if view.HasNextStep(s) {
			t.Errorf("HasNextStep(%q) = true, want false", s)
		}
	}
	if !slices.Contains(userFacing, "cli") {
		t.Fatal("cli is user-facing")
	}
}
