package provisioner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// NO TOPOLOGY-SPECIFIC HOSTNAME MAY BE COMPILED IN, and this is an AST scan
// rather than a grep so the doc comments that EXPLAIN the removal (which must
// keep naming the offending value) cannot satisfy or trip it.
//
// `langfuse-web` is a docker-compose service alias. It resolved in the compose
// topology and in no other, and as the provisioner's fallback it made every
// workspace agent on the k8s fleet retry an OTLP export against an unresolvable
// name forever — 22,224 error lines in ns/enteros-dinecall-ai and 9,011 in
// ns/enteros-minori over 24h to 2026-09-01, ~90% of each tenant's errors. The
// value now lives in docker-compose.yml and dev-start.sh, which are the two
// places where it is TRUE, and reaches the provisioner as
// MOLECULE_WORKSPACE_LANGFUSE_HOST.
func TestNoCompiledInLangfuseHostname(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "langfuse-web") {
				t.Errorf("%s: string literal %s hardcodes a docker-compose service alias as a "+
					"trace endpoint. It resolves in ONE topology; configure it via "+
					"MOLECULE_WORKSPACE_LANGFUSE_HOST instead", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	// Non-vacuity: a scan that parsed nothing would pass for the wrong reason.
	if scanned == 0 {
		t.Fatal("scanned no source files — the tripwire would pass vacuously")
	}
}

// isLoopbackHostURL gates whether a configured LANGFUSE_HOST is rewritten to the
// container-network URL. A host-loopback value (the platform's host-published
// Langfuse) is unreachable from a sibling workspace container and MUST rewrite;
// a real external target MUST be preserved.
func TestIsLoopbackHostURL(t *testing.T) {
	loopback := []string{
		"http://127.0.0.1:3001",
		"http://localhost:3001",
		"https://localhost",
		"http://[::1]:3001",
		"http://127.0.0.5:8080", // 127/8 is all loopback
	}
	for _, u := range loopback {
		if !isLoopbackHostURL(u) {
			t.Errorf("isLoopbackHostURL(%q) = false, want true (must rewrite to container URL)", u)
		}
	}
	external := []string{
		"http://langfuse-web:3000", // the container-network target itself
		"https://cloud.langfuse.com",
		"http://langfuse.internal.example.com",
		"", // unset — handled by the !set branch, not the loopback check
	}
	for _, u := range external {
		if isLoopbackHostURL(u) {
			t.Errorf("isLoopbackHostURL(%q) = true, want false (deliberate target, preserve)", u)
		}
	}
}
