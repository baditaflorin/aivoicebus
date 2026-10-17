package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const internalPrefix = "github.com/baditaflorin/aivoicebus/internal/"

// allowed lists, per internal package, the internal packages it may import
// (production code only). Dependencies point one way: pure types at the
// bottom, transport and storage in the middle, commands on top. A new package
// must be added here, with the smallest set it needs.
var allowed = map[string][]string{
	"msg":       {},
	"attach":    {"config"}, // config: the node list, so a link to a node is recognised
	"secrets":   {},
	"credstore": {},
	"config":    {},
	"assets":    {},
	"version":   {},
	"release":   {"config"},
	"identity":  {"config", "msg"},
	"view":      {"attach", "msg"},
	"store":     {"msg"},
	"client":    {"config", "msg", "view"}, // view: a node's words reach callers only as safe lines
	"server":    {"client", "config", "msg", "release", "secrets", "store", "version"},
	"hook":      {"client", "config", "discovery", "identity", "msg", "outbox", "release", "version", "view"},
	"agentcfg":  {"assets", "config", "version"},
	"discovery": {"client", "config", "release"},
	"updater":   {"client", "release"},
	"mcp":       {"attach", "client", "identity", "msg", "outbox", "tasks", "view"},
	"tasks":     {"client", "msg", "outbox"},
	"worker":    {"client", "config", "msg", "outbox", "tasks", "view"},
	"outbox":    {"client", "config", "identity", "msg", "release", "secrets", "view"},
	"cli":       {"agentcfg", "assets", "attach", "client", "config", "credstore", "discovery", "hook", "identity", "mcp", "msg", "outbox", "release", "secrets", "server", "store", "tasks", "updater", "version", "view", "worker"},
}

func TestPackageDependenciesPointOneWay(t *testing.T) {
	dirs, err := os.ReadDir("internal")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		allow, known := allowed[name]
		if !known {
			t.Errorf("internal/%s is not in the dependency table in arch_test.go", name)
			continue
		}
		files, _ := filepath.Glob(filepath.Join("internal", name, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range af.Imports {
				dep, ok := strings.CutPrefix(strings.Trim(imp.Path.Value, `"`), internalPrefix)
				if ok && !slices.Contains(allow, dep) {
					t.Errorf("%s imports internal/%s; internal/%s may only import %v", f, dep, name, allow)
				}
			}
		}
	}
}

// viewerAllowed is what the public viewer package (outside internal/, so a dashboard can import it)
// may use: the read side only, never the CLI, the hooks or the server.
var viewerAllowed = []string{"client", "msg", "store", "view"}

func TestTheViewerDependsOnTheReadSideOnly(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("viewer", "*.go"))
	if len(files) == 0 {
		t.Fatal("no viewer package")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			if dep, ok := strings.CutPrefix(strings.Trim(imp.Path.Value, `"`), internalPrefix); ok && !slices.Contains(viewerAllowed, dep) {
				t.Errorf("%s imports internal/%s; the viewer may only import %v", f, dep, viewerAllowed)
			}
		}
	}
}
