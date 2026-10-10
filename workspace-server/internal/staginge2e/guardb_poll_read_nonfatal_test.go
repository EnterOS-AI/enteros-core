package staginge2e

// Untagged proof for the property described in guardb_poll_read_nonfatal.go.
//
// The wait it protects lives behind `//go:build staging_e2e` and needs a real
// staging tenant, so it cannot be executed here. What CAN be checked here — and
// is the thing that actually regresses — is the SOURCE property: the readiness
// loop must not poll with the reader that t.Fatalf's on a transport error.
// This mirrors TestAdminCreateOrgRegistersCleanupBeforeProvisionWait, which
// likewise parses the tagged harness from the untagged gate.
//
// A comment could not do this job: the defect is a call, and only reading the
// call catches it coming back.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

const (
	guardBLiveFile = "platform_agent_mgmt_mcp_e2e_test.go"
	guardBTestName = "TestPlatformAgentMgmtMCP_Staging"

	// fatalTenantReader t.Fatalf's on a transport error; nonFatalTenantReader
	// logs and returns (0, "") so the caller can treat it as "no information".
	fatalTenantReader    = "doTenantJSON"
	nonFatalTenantReader = "doTenantJSONTimeout"
)

// parseGuardBLiveTest returns the AST of the tagged Guard B test function.
// parser.ParseFile ignores build tags, which is exactly what is wanted: the
// untagged gate gets to read the tagged source.
func parseGuardBLiveTest(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, guardBLiveFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", guardBLiveFile, err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == guardBTestName && fn.Body != nil {
			return fset, fn
		}
	}
	t.Fatalf("%s not found in %s — this guard is pinned to that function and must be "+
		"updated deliberately, not silently skipped", guardBTestName, guardBLiveFile)
	return nil, nil
}

// conciergeReadinessLoop returns the `for !online { … }` readiness wait inside
// the Guard B live test.
//
// It is identified by its CONDITION (`!online`) rather than by position, so
// inserting another loop into the test cannot make this guard silently start
// inspecting the wrong one — and a rename of `online` fails the lookup loudly
// instead of vacuously passing.
func conciergeReadinessLoop(t *testing.T, fn *ast.FuncDecl) *ast.ForStmt {
	t.Helper()
	var found *ast.ForStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		loop, ok := n.(*ast.ForStmt)
		if !ok || loop.Cond == nil {
			return true
		}
		unary, ok := loop.Cond.(*ast.UnaryExpr)
		if !ok || unary.Op != token.NOT {
			return true
		}
		if ident, ok := unary.X.(*ast.Ident); ok && ident.Name == "online" {
			found = loop
			return false
		}
		return true
	})
	if found == nil {
		t.Fatalf("the `for !online` concierge readiness loop was not found in %s. "+
			"This guard asserts a property OF that loop; if the loop was restructured or "+
			"renamed, re-point the guard deliberately — do not delete it, or the fatal "+
			"reader can come back unobserved.", guardBTestName)
	}
	return found
}

// calleeName returns the identifier a call expression calls, for plain
// (non-selector) calls. Selector calls (pkg.Fn / x.Fn) return "".
func calleeName(call *ast.CallExpr) string {
	if ident, ok := call.Fun.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// TestConciergeReadinessPollCannotFatalOnATransportError is the regression that
// matters: the readiness poll must use the NON-FATAL reader.
//
// Why not just assert "doTenantJSONTimeout is called somewhere in the test": it
// already was, twice (the A2A turn and the self-report), so that assertion was
// true BEFORE the fix and would have caught nothing. The assertion has to be
// about the readiness LOOP specifically, and it has to be a REFUSAL of the fatal
// reader, not merely a sighting of the safe one — a loop that called both would
// still be able to die on a dropped connection.
func TestConciergeReadinessPollCannotFatalOnATransportError(t *testing.T) {
	t.Parallel()

	fset, fn := parseGuardBLiveTest(t)
	loop := conciergeReadinessLoop(t, fn)

	var fatalCalls []string
	sawNonFatal := false
	ast.Inspect(loop.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch calleeName(call) {
		case fatalTenantReader:
			fatalCalls = append(fatalCalls, fset.Position(call.Pos()).String())
		case nonFatalTenantReader:
			sawNonFatal = true
		}
		return true
	})

	if len(fatalCalls) > 0 {
		t.Fatalf("the Guard B concierge readiness loop calls %s at %s. That reader t.Fatalf's on a "+
			"transport error, so Obs.ReadOK=false — the watch's documented \"a failed read is not "+
			"evidence, it only advances the budget\" arm — becomes UNREACHABLE for a dropped "+
			"connection, a TLS reset or a client timeout. One such blip in up to 60 polls over %s "+
			"would then end this HARD GATE with a bare transport error and no verdict, reverting the "+
			"staging pin and rerolling the fleet. Use %s (see guardb_poll_read_nonfatal.go).",
			fatalTenantReader, strings.Join(fatalCalls, ", "), conciergeOnlineBudget, nonFatalTenantReader)
	}
	if !sawNonFatal {
		t.Fatalf("the Guard B concierge readiness loop calls neither %s nor %s — it no longer polls "+
			"the tenant through a reader this guard can classify, so the guard is asserting nothing. "+
			"Re-point it at whatever the loop now reads with.",
			fatalTenantReader, nonFatalTenantReader)
	}
}

// TestConciergePollReadTimeoutCannotConsumeTheWholeBudget pins the per-poll
// bound against the wait it lives inside.
//
// A per-poll timeout at or above the budget would reproduce the very shape being
// removed — one hung read decides the whole wait — just without the t.Fatalf. It
// also must stay positive: a zero or negative http.Client.Timeout means NO
// timeout at all, so the poll could hang forever and the budget would never be
// evaluated again.
func TestConciergePollReadTimeoutCannotConsumeTheWholeBudget(t *testing.T) {
	t.Parallel()

	if conciergePollReadTimeout <= 0 {
		t.Fatalf("conciergePollReadTimeout=%s: a non-positive http.Client.Timeout means NO timeout, "+
			"so one poll could hang past the budget and the wait would never speak again",
			conciergePollReadTimeout)
	}
	// Several polls must fit, or a couple of slow reads eat the wait.
	if conciergePollReadTimeout*4 > conciergeOnlineBudget {
		t.Fatalf("conciergePollReadTimeout=%s leaves fewer than 4 polls inside conciergeOnlineBudget=%s "+
			"— a single hung read would then dominate the wait, which is the failure shape this file exists to remove",
			conciergePollReadTimeout, conciergeOnlineBudget)
	}
}

// TestNonFatalTenantReaderIsActuallyNonFatalOnTransportErrors closes the other
// half: the guard above is only worth anything if the reader it mandates really
// does return instead of dying. Asserted at the source, for the same reason —
// the behaviour lives behind the build tag.
//
// Specifically: inside doTenantJSONTimeout, the `if err != nil` branch that
// follows client.Do must NOT contain a t.Fatalf. If someone "tidied" that branch
// into a Fatalf, every caller — the A2A turn, the self-report diagnostic and now
// the readiness poll — would silently regain the property all three were changed
// to avoid, and the AST guard above would still pass.
func TestNonFatalTenantReaderIsActuallyNonFatalOnTransportErrors(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, guardBLiveFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", guardBLiveFile, err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == nonFatalTenantReader && d.Body != nil {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s not found in %s", nonFatalTenantReader, guardBLiveFile)
	}

	// The transport-error branch is the `if err != nil` immediately guarding a
	// `return 0, ""`. Find it by its return shape rather than by ordinal.
	var transportBranch *ast.IfStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Body == nil {
			return true
		}
		for _, stmt := range ifs.Body.List {
			ret, ok := stmt.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 2 {
				continue
			}
			lit, ok := ret.Results[0].(*ast.BasicLit)
			if ok && lit.Value == "0" {
				transportBranch = ifs
				return false
			}
		}
		return true
	})
	if transportBranch == nil {
		t.Fatalf("%s no longer has a branch that returns (0, \"\") — its non-fatal contract, which "+
			"three callers depend on, cannot be verified. Restore it or re-point this guard.",
			nonFatalTenantReader)
	}

	ast.Inspect(transportBranch.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Fatalf" || sel.Sel.Name == "Fatal" {
			t.Fatalf("%s's transport-error branch calls t.%s at %s. That reintroduces the exact "+
				"property its three callers were changed to avoid: a dropped connection ending the "+
				"HARD GATE from inside a poll or a diagnostic.",
				nonFatalTenantReader, sel.Sel.Name, fset.Position(call.Pos()))
		}
		return true
	})
}
