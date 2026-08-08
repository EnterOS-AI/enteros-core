package staginge2e

// readiness_settle_provenance_test.go — the DRIFT GUARD for TerminalSettleDefault.
//
// BLOCKER 1 (review 20596). The previous guard was:
//
//	const cpSelfHealWindow = 180 * time.Second // registry.go, db/redis.go
//	if TerminalSettleDefault != cpSelfHealWindow { ... }
//
// which compares a package-local literal to another package-local literal. The
// reviewer moved BOTH upstream constants from 180s to 420s and nothing noticed.
// A guard that cannot detect the drift it exists to detect is precisely the
// vacuous-check class this entire change is about — it was `grep -q ""` wearing
// a provenance comment.
//
// The settle window is only defensible BECAUSE it is the control plane's own
// self-heal window for this signal. If the control plane raises its grace to
// 420s and this gate keeps waiting 180s, the gate starts calling workspaces dead
// while the CP is still legitimately remediating them — manufacturing exactly
// the false reds this PR exists to remove. So the number must be bound to the
// real upstream values, by something that breaks when they move.
//
// Two bindings, one per upstream source, chosen by what each source permits:
//
//  1. db.LivenessTTL — EXPORTED, and internal/db has no non-test func init(),
//     so this package can take a real compile-time reference. The Go compiler
//     and this assertion do the guarding together; there is no literal to drift.
//
//  2. handlers.managementMCPUnloadedGrace — UNEXPORTED, and internal/handlers
//     carries non-test package init() wiring (rescue_wiring.go,
//     eic_tunnel_pool_setup.go) that must not be dragged into an e2e test
//     binary. So it is read out of the real source file by parsing the AST —
//     the same technique models.AllWorkspaceStatuses' drift gate uses, and for
//     the same reason. Reading the actual declaration is what makes it a guard;
//     a copied literal would not be.
//
// Both bindings FAIL LOUD if the constant is renamed, deleted, or moved rather
// than silently passing on a value they could not find.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// TestTerminalSettleDefault_IsBoundToTheRealLivenessTTL takes a genuine
// compile-time reference to the upstream constant. If internal/db moves
// LivenessTTL to any other value, this fails — there is no local copy to drift.
func TestTerminalSettleDefault_IsBoundToTheRealLivenessTTL(t *testing.T) {
	if TerminalSettleDefault != db.LivenessTTL {
		t.Fatalf("DRIFT: TerminalSettleDefault=%s but the control plane's workspace liveness TTL "+
			"(internal/db.LivenessTTL) is now %s.\n"+
			"The settle window is only justified because it equals the control plane's own self-heal window for "+
			"this signal. If the CP now tolerates %s of missed liveness and this gate still gives up after %s, the "+
			"gate will start calling workspaces dead while the CP is legitimately still remediating them — "+
			"manufacturing the false reds this gate exists to remove. Move TerminalSettleDefault with it, or "+
			"justify the divergence explicitly.",
			TerminalSettleDefault, db.LivenessTTL, db.LivenessTTL, TerminalSettleDefault)
	}
}

// TestTerminalSettleDefault_IsBoundToTheRealManagementMCPGrace reads the value
// out of internal/handlers' actual source. This is the semantically closest
// upstream window (it is the platform-agent management-MCP flap absorber, the
// exact signal Guard B waits on), and it is unexported, so the binding is a
// source read rather than a compile-time reference.
func TestTerminalSettleDefault_IsBoundToTheRealManagementMCPGrace(t *testing.T) {
	got, where := durationConstFromSource(t, "../handlers", "managementMCPUnloadedGrace")
	if TerminalSettleDefault != got {
		t.Fatalf("DRIFT: TerminalSettleDefault=%s but %s declares managementMCPUnloadedGrace=%s.\n"+
			"That constant IS the control plane's flap-absorption window for the platform-agent management-MCP "+
			"signal — the one this gate waits on. A gate that declares a terminal verdict faster than the "+
			"remediation it is observing will red healthy boots. Move TerminalSettleDefault with it, or justify "+
			"the divergence explicitly.",
			TerminalSettleDefault, where, got)
	}
}

// durationConstFromSource parses the Go source in dir and returns the value of
// the named duration constant, plus the file it was found in.
//
// FAIL-LOUD, never fail-open: a constant that cannot be found or cannot be
// evaluated is itself drift (a rename, a move, a change of form), and is
// reported as a failure rather than silently skipped. A guard that passes when
// it cannot find what it is guarding is not a guard.
func durationConstFromSource(t fataller, dir, name string) (time.Duration, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("cannot read %s to verify %s (err=%v, files=%d) — the drift guard must not pass when it "+
			"cannot reach the source it is guarding", dir, name, err, len(files))
	}
	// EVERY declaration is collected, never just the first match in sort order.
	//
	// Review 20599: `parser.ParseFile(…, 0)` does not evaluate build constraints,
	// and filepath.Glob returns sorted names, so a `//go:build ignore` decoy
	// sorting before the real file would have won and the guard would have
	// reported the DECOY's value — passing while the compiled constant said
	// something else. That is a guard defeated by something other than the thing
	// it guards, which is the whole defect class this package exists to refuse,
	// so ambiguity is now a FAILURE rather than a silent first-wins choice.
	//
	// It is deliberately NOT solved by teaching this reader about build tags:
	// then the reader would need to be right about constraint evaluation for the
	// guard to be trustworthy. Refusing to answer when more than one declaration
	// exists needs no such cleverness, and also catches the honest version of the
	// same hazard — a genuine second declaration behind a build tag, where the
	// value this gate depends on becomes configuration rather than a constant.
	type decl struct {
		val  time.Duration
		file string
	}
	var found []decl
	fset := token.NewFileSet()
	for _, f := range files {
		parsed, perr := parser.ParseFile(fset, f, nil, 0)
		if perr != nil {
			continue // an unparseable file elsewhere in the package is not our concern
		}
		var fail string
		ast.Inspect(parsed, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Values) != len(vs.Names) {
				return true
			}
			for i, id := range vs.Names {
				if id.Name != name {
					continue
				}
				d, derr := evalDurationExpr(vs.Values[i])
				if derr != "" {
					fail = derr
					return false
				}
				found = append(found, decl{d, f})
			}
			return true
		})
		if fail != "" {
			t.Fatalf("found %s in %s but could not evaluate its value: %s. The drift guard must be updated to "+
				"understand the new form rather than left silently passing.", name, f, fail)
		}
	}
	switch len(found) {
	case 1:
		return found[0].val, found[0].file
	case 0:
		t.Fatalf("constant %s was NOT found anywhere in %s. It was renamed, moved or deleted — which is itself "+
			"the drift this guard exists to catch, so this is a failure, not a skip.", name, dir)
	default:
		var where []string
		for _, d := range found {
			where = append(where, fmt.Sprintf("%s=%s", d.file, d.val))
		}
		t.Fatalf("constant %s is declared %d times in %s (%s). This reader does not evaluate build constraints, "+
			"so it CANNOT know which one the compiler uses — and picking one would make the guard depend on file "+
			"sort order rather than on the value it is guarding. Refusing to answer.",
			name, len(found), dir, strings.Join(where, ", "))
	}
	return 0, ""
}

// evalDurationExpr evaluates the small set of forms a duration constant is
// written in: `<int> * time.<Unit>` (either operand order) and a bare
// `time.<Unit>`. Anything else returns a reason string so the caller can fail
// loud instead of guessing.
func evalDurationExpr(e ast.Expr) (time.Duration, string) {
	units := map[string]time.Duration{
		"Nanosecond":  time.Nanosecond,
		"Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond,
		"Second":      time.Second,
		"Minute":      time.Minute,
		"Hour":        time.Hour,
	}
	unitOf := func(x ast.Expr) (time.Duration, bool) {
		sel, ok := x.(*ast.SelectorExpr)
		if !ok {
			return 0, false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "time" {
			return 0, false
		}
		u, ok := units[sel.Sel.Name]
		return u, ok
	}
	intOf := func(x ast.Expr) (int64, bool) {
		lit, ok := x.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseInt(lit.Value, 0, 64)
		return n, err == nil
	}

	if u, ok := unitOf(e); ok {
		return u, ""
	}
	be, ok := e.(*ast.BinaryExpr)
	if !ok || be.Op != token.MUL {
		return 0, "expression is not `<int> * time.<Unit>` or a bare `time.<Unit>`"
	}
	if n, ok := intOf(be.X); ok {
		if u, ok := unitOf(be.Y); ok {
			return time.Duration(n) * u, ""
		}
	}
	if n, ok := intOf(be.Y); ok {
		if u, ok := unitOf(be.X); ok {
			return time.Duration(n) * u, ""
		}
	}
	return 0, "multiplication operands are not an integer literal and a time unit"
}

// The reader itself must be able to FAIL. A parser that silently returns zero,
// or that matches any identifier, would hand back a green guard on drift — the
// same vacuum in a new place. These pin that it discriminates.
func TestDurationConstFromSource_ReadsRealValuesAndDiscriminates(t *testing.T) {
	// A known-good read against a different real upstream constant proves the
	// reader works on this repo's actual source, not just on a fixture.
	got, where := durationConstFromSource(t, "../db", "LivenessTTL")
	if got != db.LivenessTTL {
		t.Fatalf("the source reader returned %s for LivenessTTL in %s but the compiled value is %s — "+
			"the reader is not reading what the compiler sees", got, where, db.LivenessTTL)
	}
	// And the expression evaluator must reject forms it does not understand
	// rather than inventing a value.
	for _, src := range []string{"someOtherConst", "1 << 20", "time.Second / 2"} {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("test fixture %q did not parse: %v", src, err)
		}
		if _, reason := evalDurationExpr(expr); reason == "" {
			t.Fatalf("evalDurationExpr accepted %q; it must refuse forms it cannot evaluate so the guard fails "+
				"loud instead of guessing", src)
		}
	}
	// Positive control: the forms it DOES understand must evaluate correctly.
	for src, want := range map[string]time.Duration{
		"180 * time.Second": 180 * time.Second,
		"time.Minute * 7":   7 * time.Minute,
		"time.Hour":         time.Hour,
	} {
		expr, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatalf("fixture %q: %v", src, err)
		}
		d, reason := evalDurationExpr(expr)
		if reason != "" || d != want {
			t.Fatalf("evalDurationExpr(%q) = (%s, %q), want %s", src, d, reason, want)
		}
	}
}

// A missing constant must FAIL, not pass. Proven against a name that certainly
// does not exist, using the same code path the real guards use.
func TestDurationConstFromSource_FailsLoudWhenTheConstantIsGone(t *testing.T) {
	sub := &fatalRecorder{}
	func() {
		defer func() { recover() }() // t.Fatalf on the fake calls runtime.Goexit
		durationConstFromSource(sub, "../handlers", "thisConstantDoesNotExistAnywhere")
	}()
	if !sub.failed {
		t.Fatalf("a constant that does not exist was NOT reported as a failure — the guard would pass while "+
			"guarding nothing (message: %q)", sub.msg)
	}
	if !strings.Contains(sub.msg, "NOT found") {
		t.Fatalf("the failure must say the constant was not found: %q", sub.msg)
	}
}

// fataller is the slice of *testing.T durationConstFromSource needs. Taking an
// interface rather than *testing.T is what lets the "must fail loud" property
// above be PROVEN on the same code path the real guards run, instead of
// asserted in a comment.
type fataller interface {
	Helper()
	Fatalf(format string, args ...any)
}

// fatalRecorder is a fataller that records the failure and unwinds, standing in
// for *testing.T so a guard failure can be observed instead of ending the test.
type fatalRecorder struct {
	failed bool
	msg    string
}

func (f *fatalRecorder) Helper() {}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
	panic("fatalRecorder: " + f.msg)
}

// A BUILD-EXCLUDED DECOY must defeat the guard's ANSWER, not the guard.
//
// Review 20599 found the one attack the reader did not survive: parser.ParseFile
// with mode 0 ignores build constraints and filepath.Glob returns sorted names,
// so a `//go:build ignore` file sorting before registry.go and carrying the OLD
// value would win first-match and the guard would pass while the compiled
// constant said something else. Contrived — Go rejects duplicate constants
// unless one is build-excluded, so drift alone cannot produce it — but a guard
// defeated by file sort order is exactly the shape this package refuses.
//
// The fixture is a real temp directory rather than a planted file in
// internal/handlers, so the proof is permanent and cannot itself rot the
// package it is guarding.
func TestDurationConstFromSource_RefusesABuildExcludedDecoy(t *testing.T) {
	dir := t.TempDir()
	// "aaa_decoy.go" sorts BEFORE "registry.go", which is precisely the ordering
	// that made first-match unsafe.
	mustWrite(t, filepath.Join(dir, "aaa_decoy.go"), `//go:build ignore

package handlers

import "time"

const managementMCPUnloadedGrace = 180 * time.Second
`)
	mustWrite(t, filepath.Join(dir, "registry.go"), `package handlers

import "time"

const managementMCPUnloadedGrace = 420 * time.Second
`)

	sub := &fatalRecorder{}
	func() {
		defer func() { recover() }()
		durationConstFromSource(sub, dir, "managementMCPUnloadedGrace")
	}()
	if !sub.failed {
		t.Fatalf("a build-excluded decoy carrying the OLD value silently won: the guard returned an answer "+
			"instead of refusing. It would then pass while the compiled constant reads 420s (message: %q)", sub.msg)
	}
	if !strings.Contains(sub.msg, "declared 2 times") {
		t.Fatalf("the refusal must name the ambiguity it found: %q", sub.msg)
	}
	// And it must have SEEN both values, so an operator can tell decoy from real.
	for _, want := range []string{"3m0s", "7m0s"} {
		if !strings.Contains(sub.msg, want) {
			t.Fatalf("the refusal must report every declaration it found (missing %s): %q", want, sub.msg)
		}
	}
}

// The unambiguous case must still ANSWER — the refusal above is worthless if it
// also fires on the normal single-declaration shape.
func TestDurationConstFromSource_AnswersWhenThereIsExactlyOneDeclaration(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "aaa_unrelated.go"), `package handlers

import "time"

const somethingElse = 99 * time.Second
`)
	mustWrite(t, filepath.Join(dir, "registry.go"), `package handlers

import "time"

const managementMCPUnloadedGrace = 420 * time.Second
`)
	got, where := durationConstFromSource(t, dir, "managementMCPUnloadedGrace")
	if got != 420*time.Second {
		t.Fatalf("got %s from %s, want 7m0s", got, where)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
