package handlers

// delegation_status_ssot_test.go — the guard #4314 never had.
//
// The original bug was not a typo. It was a HAND-TYPED SQL status list: the
// sweeper wrote `stuck`, the mail digest counted
// `status IN ('queued','dispatched','in_progress')`, and a wedged delegation
// silently vanished from the caller's "awaiting reply" count. The one case an
// operator most needs to see was the one the platform made invisible.
//
// Fixing the list would fix that instance and leave the CLASS wide open: the next
// state anyone adds drops out of whichever IN-list they forget. So the vocabulary
// has one definition and every consumer derives from it, and these tests pin that
// property rather than any particular list.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestDelegationStates_VocabularyIsPartitioned(t *testing.T) {
	// Every state the CHECK constraint permits must be classified EXACTLY once.
	// A state in neither set is invisible to every consumer; a state in both is
	// simultaneously over and awaiting an answer.
	checkConstraint := []string{"queued", "dispatched", "in_progress", "completed", "failed", "stuck"}

	seen := map[string]int{}
	for _, s := range DelegationInFlightStates {
		seen[s]++
	}
	for _, s := range DelegationTerminalStates {
		seen[s]++
	}
	for _, s := range checkConstraint {
		switch seen[s] {
		case 0:
			t.Errorf("status %q is permitted by the schema CHECK but is in NEITHER "+
				"DelegationInFlightStates nor DelegationTerminalStates — it is invisible to "+
				"every consumer, which is exactly how `stuck` caused #4314", s)
		case 1: // correct
		default:
			t.Errorf("status %q is in BOTH the in-flight and terminal sets", s)
		}
		delete(seen, s)
	}
	for s := range seen {
		t.Errorf("status %q is classified but the schema CHECK does not permit it", s)
	}
}

func TestDelegationStates_StuckIsInFlightNotTerminal(t *testing.T) {
	// The specific regression. A wedged target has NOT answered, so the caller is
	// still waiting — and the "⚠ the target agent may have an issue" warning is
	// rendered from exactly this row. Classifying it terminal both hides the
	// warning and (via the forward-only guard) corrupts the ledger when the target
	// comes back.
	inFlight := strings.Join(DelegationInFlightStates, ",")
	if !strings.Contains(inFlight, "stuck") {
		t.Fatal("`stuck` must be IN-FLIGHT: the target has not answered, so the caller " +
			"is still awaiting a reply and the ⚠ warning is rendered from this row (#4314)")
	}
	for _, s := range DelegationTerminalStates {
		if s == "stuck" {
			t.Fatal("`stuck` must NOT be terminal — it is recoverable; the target can come " +
				"back and its a2a_queue message still deliver")
		}
	}
}

// TestNoHandTypedDelegationStatusList is the class-level guard.
//
// THE FIRST VERSION OF THIS GUARD WAS NEARLY USELESS. It matched only the exact
// SQL shape `status IN ('queued','dispatched','in_progress'` — so it proved that
// the two lines already fixed stayed fixed, and was blind to every other way the
// same drift expresses itself. A re-review found THREE live hand-typed lists it
// could not see, in this very diff:
//
//	delegation_ledger.go  Heartbeat's `status NOT IN ('completed','failed','stuck')`
//	                      — a NOT IN, and one that still called `stuck` terminal,
//	                      200 lines under the vocabulary declaring it recoverable
//	admin_delegations.go  `"in_flight": {"queued","dispatched","in_progress"}` —
//	                      a Go SLICE, and it backed the operator dashboard's DEFAULT
//	                      view, so the operator looking for a wedged agent saw
//	                      everything except the wedged delegations
//	delegation.go         a six-way `switch status { case "queued", ... }`
//
// A guard that only recognises one syntax is a guard against typos, not against
// the bug. So this now keys on the TOKENS, not the shape: `in_progress` and
// `stuck` appear ONLY in the delegations lifecycle, so any non-comment occurrence
// of either outside the SSOT file is a second definition of the vocabulary,
// whatever syntax it is wearing.
func TestNoHandTypedDelegationStatusList(t *testing.T) {
	// THE PREMISE WAS INVERTED, AND REVIEW REINTRODUCED #4314 STRAIGHT THROUGH IT.
	//
	// v3 flagged a hand-typed set only if it CONTAINED a delegations-only word
	// (in_progress / stuck). But a drifted list is defined by what it OMITS — and
	// #4314 *is* the omission of `stuck`. So the guard keyed on exactly the tokens
	// the bug removes. This sailed past it, in mail_summary.go, on the delegations
	// table, with the whole suite green:
	//
	//	const awaitingClause = "status IN ('queued','dispatched')"
	//
	// which is #4314 verbatim: wedged and in-progress delegations vanish from the
	// caller's "awaiting reply" count. A guard you can restore the original bug
	// through is not a guard.
	//
	// So the rule is now DEFAULT-DENY. Any hand-typed SET of two or more delegation
	// states is a second copy of the vocabulary, wherever it appears and whichever
	// states it happens to contain. Copies drift; that drift is the bug.
	//
	// Legitimate exceptions exist — a2a_queue is a DIFFERENT table whose own
	// vocabulary overlaps ours — and they are opted out ONE BY ONE with a
	//
	//	//ssot:allow-status-set <why>
	//
	// marker on or just above the construct. That is deliberate: an exemption then
	// appears in the diff, where a reviewer sees it and asks why. Guessing from
	// context — which table a literal "probably" belongs to, whether the enclosing
	// func is "probably" about delegations — is what let the bug back in.
	//
	// KNOWN LIMIT, stated rather than papered over: this sees hand-typed LITERALS.
	// A set assembled at runtime (fmt.Sprintf over a slice, a query builder, states
	// read from config) is invisible to it. The SSOT accessors exist so that no one
	// needs to do that; if someone does, this will not catch them.
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	isState := map[string]bool{}
	for _, st := range DelegationAllStates {
		isState[st] = true
	}

	litState := func(n ast.Node) (string, bool) {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(bl.Value)
		if err != nil || !isState[v] {
			return "", false
		}
		return v, true
	}

	checked := 0
	for _, f := range files {
		// delegation_ledger.go IS the vocabulary; it alone may spell it out.
		if strings.HasSuffix(f, "_test.go") || f == "delegation_ledger.go" {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		checked++

		// Lines carrying an explicit opt-out. The marker exempts the WHOLE comment
		// group it appears in: the reason usually runs to several lines, and the
		// construct sits below the LAST of them — so marking only the marker's own
		// line meant a multi-line exemption never matched the node beneath it.
		allow := map[int]bool{}
		for _, cg := range file.Comments {
			marked := false
			for _, c := range cg.List {
				if strings.Contains(c.Text, "ssot:allow-status-set") {
					marked = true
				}
			}
			if !marked {
				continue
			}
			for l := fset.Position(cg.Pos()).Line; l <= fset.Position(cg.End()).Line; l++ {
				allow[l] = true
			}
		}
		exempt := func(n ast.Node) bool {
			lo := fset.Position(n.Pos()).Line
			hi := fset.Position(n.End()).Line
			for l := lo - 1; l <= hi; l++ { // the marker may sit just above
				if allow[l] {
					return true
				}
			}
			return false
		}

		covered := map[ast.Node]bool{}
		report := func(n ast.Node, shape string, found map[string]bool) {
			if exempt(n) {
				return
			}
			t.Errorf("%s: hand-typed delegation status SET %v in a %s.\n"+
				"    This is the #4314 bug class: a SECOND COPY of the vocabulary, and copies "+
				"drift — #4314 was a list that omitted `stuck`.\n"+
				"    Derive it from DelegationInFlightStates / DelegationTerminalStates / "+
				"DelegationAllStates, or sqlInFlightStates() / sqlTerminalStates() / "+
				"IsTerminalDelegationStatus().\n"+
				"    If this genuinely is NOT the delegations vocabulary (a2a_queue is a "+
				"different table), mark it: //ssot:allow-status-set <why>",
				fset.Position(n.Pos()), sortedStateKeys(found), shape)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				// SQL — BY PREDICATE POSITION, NOT BY SYNTAX.
				//
				// v4 matched only `status [NOT] IN (...)`, and review broke it twice
				// over. BOTH of these are #4314 verbatim and both sailed through:
				//
				//   WHERE status = ANY (ARRAY['queued','dispatched'])
				//   WHERE (status = 'queued' OR status = 'dispatched')
				//
				// The first is not contrived — it is EXACTLY what Postgres renders an
				// IN-list as. `pg_indexes.indexdef` prints our own migration's
				// predicate that way, so anyone copy-pasting out of `\d+` writes the
				// escaping form BY DEFAULT. Enumerating syntaxes is unwinnable; every
				// one I miss is silent. So state the invariant instead:
				//
				//   A SQL statement may name AT MOST ONE state in PREDICATE position.
				//
				// One guard state is a legitimate transition (`SET status='completed'
				// ... WHERE status='queued'` — the CAS). TWO OR MORE states being
				// tested is a hand-typed copy of the vocabulary, whatever syntax spells
				// it. States in ASSIGNMENT position (`SET status='x'`, the value being
				// written) are not a set and are excluded.
				if node.Kind != token.STRING {
					return true
				}
				// A lone string literal. If it is one PIECE of a `+` concatenation, the
				// concat arm below folds the whole chain and owns the report — skip it
				// here so we neither double-count nor split a predicate across pieces.
				if covered[node] {
					return true
				}
				sql, err := strconv.Unquote(node.Value)
				if err != nil {
					return true
				}
				if found := statesInPredicatePosition(sql); len(found) >= 2 {
					report(node, "SQL predicate", found)
				}

			case *ast.CompositeLit:
				found := map[string]bool{}
				// Slice/array elements: always a set.
				for _, e := range node.Elts {
					if st, ok := litState(e); ok {
						found[st] = true
					}
				}
				// Map KEYS, but only when the map is a SET or a COUNTER
				// (map[string]bool, map[string]int...). There the keys ARE the
				// vocabulary — `Stats: map[string]int{"queued":0,...}` minus `stuck` is
				// #4314 in the operator dashboard, and review proved v3 missed it.
				//
				// For any other value type the keys are names, not a vocabulary:
				// `statusFilters` is map[string][]string whose keys are UI tab labels
				// that happen to coincide with state names. Counting those would be
				// noise, and a noisy guard gets deleted. ast.Inspect still descends into
				// the VALUES, so a hand-typed []string{...} in there is caught as a slice.
				if mt, ok := node.Type.(*ast.MapType); ok && isSetOrCounter(mt.Value) {
					for _, e := range node.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if st, ok := litState(kv.Key); ok {
								found[st] = true
							}
						}
					}
				}
				if len(found) >= 2 {
					report(node, "Go slice/set literal", found)
				}

			case *ast.CaseClause:
				found := map[string]bool{}
				for _, e := range node.List {
					if st, ok := litState(e); ok {
						found[st] = true
					}
				}
				if len(found) >= 2 {
					report(node, "switch-case list", found)
				}

			case *ast.BinaryExpr:
				// SQL PREDICATE SPLIT ACROSS A `+` CONCATENATION. The BasicLit arm scans
				// each literal on its own, so `("... status = 'queued'" + " OR status =
				// 'dispatched'")` puts one state in each node and neither trips the
				// two-state rule — an ordinary line-wrap walks #4314 straight through.
				// (Review's third escape, after two "now it's principled" rewrites: any
				// per-literal guard is a step behind Go's expressiveness. The DERIVATION
				// — sqlInFlightStates() — is the real fix; this guard is a backstop.)
				//
				// So fold the whole `+` chain of string literals into one buffer, mark
				// its pieces covered so the BasicLit arm skips them, and analyse the
				// concatenation as a single SQL string.
				if node.Op == token.ADD {
					if folded, pieces, ok := foldStringConcat(node); ok {
						for _, pc := range pieces {
							covered[pc] = true
						}
						if found := statesInPredicatePosition(folded); len(found) >= 2 {
							report(node, "SQL predicate (concatenated)", found)
						}
						return true
					}
				}
				// `s != "completed" && s != "failed"` — a set spelled with operators.
				if covered[node] || (node.Op != token.LAND && node.Op != token.LOR) {
					return true
				}
				found := map[string]bool{}
				ast.Inspect(node, func(c ast.Node) bool {
					if be, ok := c.(*ast.BinaryExpr); ok && be != node {
						covered[be] = true
					}
					if st, ok := litState(c); ok {
						found[st] = true
					}
					return true
				})
				if len(found) >= 2 {
					report(node, "comparison chain", found)
				}
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("the guard inspected ZERO files — it can never fail, so it proves nothing")
	}
}

// isSetOrCounter reports whether a map's VALUE type makes its KEYS a vocabulary:
// map[string]bool is a set, map[string]int is a per-state counter. Both are second
// copies of the status list. map[string][]string is a lookup whose keys are names.
func isSetOrCounter(v ast.Expr) bool {
	id, ok := v.(*ast.Ident)
	if !ok {
		return false
	}
	switch id.Name {
	case "bool", "int", "int64", "uint64", "float64":
		return true
	}
	return false
}

// sqlStateRE finds quoted delegation states inside a SQL string.
var sqlStateRE = regexp.MustCompile(`'(queued|dispatched|in_progress|completed|failed|stuck)'`)

// sqlAssignRE matches a state literal in ASSIGNMENT (value) position — a state the
// statement WRITES, not one it TESTS. Those are excluded before the set rule runs.
//
// Three value positions exist:
//
//	SET status = 'completed'                              -- plain assignment
//	SET status = CASE WHEN … THEN 'failed' ELSE 'queued'  -- conditional assignment
//	                                                         END
//
// The CASE form matters: `MarkQueueItemFailed` writes exactly that, and its two
// literals are the two values it might STORE, not a hand-typed set of states it
// classifies by. A THEN/ELSE literal is always a value — a predicate inside the same
// CASE lives in the WHEN, which is still scanned. Missing this made the guard red on
// the clean tree, and a guard that cries wolf on correct code gets deleted, taking
// the real protection with it.
//
// KNOWN BLIND SPOT (LOW): this exclusion is CONTEXT-FREE, so it also blanks THEN/ELSE
// literals in PREDICATE position — `WHERE status = CASE WHEN $1 IS NULL THEN 'queued'
// ELSE 'dispatched' END` slips through. Nobody writes an in-flight filter that way
// (the searched-CASE forms people DO write put the states in the WHEN/comparand, which
// is still scanned), so it has never been a natural #4314. Left as-is on purpose: the
// DERIVATION (sqlInFlightStates()) is the real fix and this guard is a backstop that
// trails Go/SQL expressiveness — see the file header. Tightening THEN/ELSE to require
// a preceding `SET`/`WHEN...THEN` would risk re-reddening the clean tree for no live
// bug.
var sqlAssignRE = regexp.MustCompile(
	`(?is)(?:SET\s+status\s*=\s*|\bTHEN\s+|\bELSE\s+)'(queued|dispatched|in_progress|completed|failed|stuck)'`)

// statesInPredicatePosition returns every delegation-status literal a SQL string
// TESTS (as opposed to WRITES). Assignment-position literals — `SET status='x'`, and
// `THEN`/`ELSE` inside a conditional assignment — are excluded via sqlAssignRE. This
// is the whole invariant: one state tested = a legitimate transition guard; two or
// more = a hand-typed second copy of the vocabulary, in whatever syntax.
func statesInPredicatePosition(sql string) map[string]bool {
	assigned := map[int]bool{}
	for _, m := range sqlAssignRE.FindAllStringSubmatchIndex(sql, -1) {
		assigned[m[2]] = true // start offset of the captured (written) state literal
	}
	found := map[string]bool{}
	for _, m := range sqlStateRE.FindAllStringSubmatchIndex(sql, -1) {
		if assigned[m[2]] {
			continue
		}
		found[sql[m[2]:m[3]]] = true
	}
	return found
}

// foldStringConcat flattens a `+` chain whose leaves are ALL string literals into the
// single string they concatenate to, returning the leaf nodes so the caller can mark
// them covered. Returns ok=false the moment any leaf is not a plain string literal
// (a variable, a call) — a runtime-assembled predicate is out of scope for a static
// guard and is the guard's stated known-limit.
func foldStringConcat(n ast.Node) (string, []*ast.BasicLit, bool) {
	switch e := n.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", nil, false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", nil, false
		}
		return v, []*ast.BasicLit{e}, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", nil, false
		}
		ls, lp, lok := foldStringConcat(e.X)
		rs, rp, rok := foldStringConcat(e.Y)
		if !lok || !rok {
			return "", nil, false
		}
		return ls + rs, append(lp, rp...), true
	case *ast.ParenExpr:
		return foldStringConcat(e.X)
	}
	return "", nil, false
}

func sortedStateKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMigrationIndexPredicatesMatchTheVocabulary closes the one hole the Go SSOT
// cannot reach: SQL in a migration file, where DelegationInFlightStates is not in
// scope.
//
// Both delegations partial indexes hand-typed the in-flight list and so omitted
// `stuck`. A partial index cannot serve a query whose predicate is WIDER than the
// index's — so the moment DELEGATION_LEDGER_WRITE fills the table, the sweeper
// (every 5 min, fleet-wide) and mail_summary (every idle tick, per workspace) both
// fall back to a Seq Scan. Same hand-typed-list defect as #4314, different language.
func TestMigrationIndexPredicatesMatchTheVocabulary(t *testing.T) {
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found (glob err %v) — the guard would be vacuous", err)
	}
	sort.Strings(files) // later migrations DROP + recreate: the last definition wins

	nameRE := regexp.MustCompile(`(?is)CREATE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	predicate := map[string][]string{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(string(src), ";") {
			if !strings.Contains(strings.ToUpper(stmt), "CREATE INDEX") ||
				!strings.Contains(stmt, " delegations") {
				continue
			}
			m := nameRE.FindStringSubmatch(stmt)
			if m == nil || !strings.Contains(m[1], "inflight") {
				continue // only the in-flight partial indexes make this claim
			}
			found := []string{}
			for _, st := range sqlStateRE.FindAllStringSubmatch(stmt, -1) {
				found = append(found, st[1])
			}
			sort.Strings(found)
			predicate[m[1]] = found
		}
	}

	if len(predicate) < 2 {
		t.Fatalf("expected the two in-flight partial indexes, found %d (%v) — if they "+
			"were renamed, update this guard; do not delete it", len(predicate), predicate)
	}

	want := append([]string{}, DelegationInFlightStates...)
	sort.Strings(want)
	for name, got := range predicate {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("index %s filters on %v, but the in-flight vocabulary is %v. A partial "+
				"index narrower than the query predicate is NOT USED — the sweeper and the "+
				"idle digest silently fall back to a Seq Scan.", name, got, want)
		}
	}
}

// TestStatsInitialisesEveryState USED TO LIVE HERE AND WAS VACUOUS: it built a
// map[string]int from DelegationAllStates *inside the test* and asserted len == 6. It
// never touched the handler. It asserted that a loop I had just written loops — true
// no matter how broken admin_delegations.Stats became.
//
// The property it was reaching for is now enforced where it belongs:
// TestNoHandTypedDelegationStatusList flags a hand-typed map[string]int of states
// (a per-state counter IS a copy of the vocabulary), and the handler derives its map
// from DelegationAllStates. Deleted rather than left as a green checkmark next to
// nothing.

// TestAdminDefaultViewShowsStuck — the operator's DEFAULT delegations view.
//
// A wedged agent is the single thing an operator opens this dashboard to find.
// The hand-typed `in_flight` list omitted `stuck`, so the default view showed
// everything EXCEPT the wedged delegations. Same bug as the digest's, wearing a UI.
func TestAdminDefaultViewShowsStuck(t *testing.T) {
	inFlight, ok := statusFilters["in_flight"]
	if !ok {
		t.Fatal("statusFilters has no in_flight — that is the DEFAULT query")
	}
	found := false
	for _, s := range inFlight {
		if s == "stuck" {
			found = true
		}
	}
	if !found {
		t.Errorf("the operator's DEFAULT delegations view (%v) hides `stuck` — the one "+
			"state they opened the dashboard to find (#4314)", inFlight)
	}
}
