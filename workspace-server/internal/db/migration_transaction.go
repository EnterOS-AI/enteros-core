package db

// migration_transaction.go — how ONE migration file is applied, and why the
// answer is not "wrap everything in a transaction".
//
// # THE DEFECT (rehearsal migration mig-rehearsal-a41c7, 2026-08-07)
//
// RunMigrations applied a migration and recorded it in two separate
// transactions:
//
//	DB.Exec(<file>)                                 -- commit 1
//	DB.Exec("INSERT INTO schema_migrations ...")    -- commit 2
//
// A tenant killed between them — the kubelet's liveness probe did exactly this
// on its first boot on replicated storage — leaves the DDL applied and
// unrecorded. Every later boot re-runs the file:
//
//	Migrations failed: exec 20260624000000_push_tokens.up.sql:
//	  pq: relation "push_tokens" already exists
//
// which is a permanent CrashLoopBackOff. Worse, on a substrate migration it
// deadlocks: the control plane waits for tenant readiness before the Transfer
// step, and Transfer (`pg_restore --clean --if-exists`) is the only thing that
// would repair the schema.
//
// # WHY NOT SIMPLY WRAP EVERY FILE
//
// Because three of the 117 forward migrations are not wrappable, and finding
// that out in production is how this class of fix causes its own outage:
//
//   - 20260506000000_workspaces_unique_parent_name.up.sql runs
//     CREATE UNIQUE INDEX CONCURRENTLY. Postgres REFUSES it inside any
//     transaction block ("cannot run inside a transaction block"). A blanket
//     wrap breaks every fresh tenant at that file.
//   - Eleven forward migrations (043, 20260518000000, 20260611110000,
//     20260715090000, 20260715120000, 20260717000000, 20260725120000,
//     20260726120000, 20260726120100, 20260727120000 and their kin) open and
//     COMMIT their OWN transaction. Nesting one inside an outer transaction
//     makes its COMMIT close the OUTER one — after which the ledger INSERT is
//     no longer in the transaction it was meant to be atomic with. The wrap
//     would look applied and buy nothing, which is worse than not wrapping.
//
// # THE POLICY
//
//	SELF-MANAGED    a file whose transaction control is exactly one leading
//	                BEGIN and one trailing COMMIT is NORMALISED: those two
//	                statements are removed and the body runs inside the
//	                runner's transaction, together with the ledger row. SET
//	                LOCAL inside it then scopes to that same transaction, which
//	                is what the migration author wrote it to do. Anything more
//	                elaborate — a mid-file COMMIT, a SAVEPOINT, a ROLLBACK — is
//	                REFUSED rather than guessed at.
//	CONCURRENTLY    genuinely cannot be atomic with anything. Applied outside a
//	                transaction, exactly as before, and its resumability comes
//	                from being IDEMPOTENT instead
//	                (CREATE ... CONCURRENTLY IF NOT EXISTS). That is not a hope:
//	                migration_transaction_test.go fails if any non-wrappable
//	                migration in the tree is not idempotent.
//	EVERYTHING ELSE one transaction: the file's SQL and its ledger row commit
//	                together, so an interrupted boot is always resumable.
//
// # WHY THE CLASSIFIER MASKS BEFORE IT LOOKS
//
// Two migrations discuss CONCURRENTLY at length in their COMMENTS while
// deliberately not using it (048 and 20260714120000 both explain why they build
// the index plainly). A classifier that greps the raw text would mark both
// non-wrappable and silently give up atomicity for them. Four migrations use
// dollar-quoted function bodies that contain semicolons and keywords. So the
// classifier first blanks every comment and every literal — preserving byte
// offsets, so the BEGIN/COMMIT it later removes are removed from the right
// place — and only then reads the SQL.

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
)

// errKilledMidMigration is what the crash seam returns. It exists so the
// integration gate can reproduce the kubelet's kill at the exact instruction
// boundary the rehearsal died on; production never sets the seam.
var errKilledMidMigration = errors.New("migration boot killed after DDL, before the ledger row (test seam)")

// migrationCrashAfterDDL is nil in production. It is the ONLY way to observe
// the atomicity property from a test: the property is about what survives an
// abrupt death between two writes, and nothing short of dying between them
// measures it.
var migrationCrashAfterDDL func(filename string) error

// migrationPlan is how one file will be applied.
type migrationPlan struct {
	// File is the base filename — the ledger key.
	File string
	// Body is the SQL actually executed. It differs from the file's bytes only
	// when a self-managed BEGIN/COMMIT pair was removed.
	Body string
	// Wrappable reports whether Body and the ledger row can commit together.
	Wrappable bool
	// NotWrappableBecause is stated whenever Wrappable is false, so the boot log
	// says which migrations are NOT atomic and why.
	NotWrappableBecause string
}

// planMigration decides how a file is applied, or REFUSES it.
//
// A refusal is deliberate. The alternative — falling back to the old
// two-transaction shape for anything the classifier does not recognise — would
// make an unfamiliar migration silently lose the guarantee, which is the exact
// failure this file closes.
func planMigration(filename, content string) (migrationPlan, error) {
	p := migrationPlan{File: filename, Body: content}
	masked := maskNonCode(content)

	if concurrentlyRe.MatchString(masked) {
		p.Wrappable = false
		p.NotWrappableBecause = "it builds an index CONCURRENTLY, which Postgres refuses inside a transaction block"
		return p, nil
	}

	stmts := statementHeads(masked)
	var txCtl []statementHead
	for _, s := range stmts {
		if isTransactionControl(s.Word) {
			txCtl = append(txCtl, s)
		}
	}
	switch len(txCtl) {
	case 0:
		p.Wrappable = true
		return p, nil
	case 2:
		opensFirst := isTransactionOpen(txCtl[0].Word) && txCtl[0].Index == 0
		closesLast := isTransactionClose(txCtl[1].Word) && txCtl[1].Index == len(stmts)-1
		if opensFirst && closesLast {
			p.Wrappable = true
			p.Body = blankRanges(content, txCtl[0].Start, txCtl[0].End, txCtl[1].Start, txCtl[1].End)
			return p, nil
		}
	}
	words := make([]string, 0, len(txCtl))
	for _, s := range txCtl {
		words = append(words, s.Word)
	}
	return migrationPlan{}, fmt.Errorf(
		"migration %s manages its own transaction in a shape this runner will not nest or normalise (%s). "+
			"A migration's DDL and its schema_migrations row MUST commit together or an interrupted boot "+
			"is unrecoverable; rewrite the file so its only transaction control is one leading BEGIN and "+
			"one trailing COMMIT, or none at all",
		filename, strings.Join(words, ", "))
}

var concurrentlyRe = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)

// statementHead is one top-level statement: where it starts, where its
// terminating semicolon ends, and its leading keyword (upper-cased).
type statementHead struct {
	Index int
	Start int
	End   int
	Word  string
}

// statementHeads splits MASKED sql on top-level semicolons and reports the
// leading keyword of each non-empty statement. Offsets index the ORIGINAL
// string, because masking preserves length.
func statementHeads(masked string) []statementHead {
	var out []statementHead
	start := 0
	for i := 0; i <= len(masked); i++ {
		if i < len(masked) && masked[i] != ';' {
			continue
		}
		end := i
		if i < len(masked) {
			end = i + 1 // include the semicolon
		}
		seg := masked[start:end]
		if w := leadingWord(seg); w != "" {
			out = append(out, statementHead{Index: len(out), Start: start + leadingOffset(seg), End: end, Word: w})
		}
		start = i + 1
	}
	return out
}

func leadingOffset(seg string) int {
	for i := 0; i < len(seg); i++ {
		if !isSpaceByte(seg[i]) {
			return i
		}
	}
	return 0
}

func leadingWord(seg string) string {
	i := leadingOffset(seg)
	j := i
	for j < len(seg) && isWordByte(seg[j]) {
		j++
	}
	if j == i {
		return ""
	}
	return strings.ToUpper(seg[i:j])
}

func isSpaceByte(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isTransactionControl(word string) bool {
	switch word {
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "ABORT":
		return true
	}
	return false
}
func isTransactionOpen(word string) bool  { return word == "BEGIN" || word == "START" }
func isTransactionClose(word string) bool { return word == "COMMIT" || word == "END" }

// blankRanges replaces the given [start,end) byte ranges with spaces, keeping
// newlines so an error's line numbers still match the file on disk.
func blankRanges(s string, bounds ...int) string {
	b := []byte(s)
	for i := 0; i+1 < len(bounds); i += 2 {
		for j := bounds[i]; j < bounds[i+1] && j < len(b); j++ {
			if b[j] != '\n' {
				b[j] = ' '
			}
		}
	}
	return string(b)
}

// maskNonCode returns s with every comment and every literal replaced by
// spaces, preserving BOTH length and newlines so offsets and line numbers stay
// valid.
//
// Handles: -- line comments, /* */ block comments (Postgres nests them),
// 'single-quoted' strings with the '' escape, "quoted identifiers", and
// $tag$ dollar-quoted $tag$ bodies — the last because four migrations define
// functions whose bodies contain semicolons and SQL keywords that would
// otherwise be read as top-level statements.
func maskNonCode(s string) string {
	b := []byte(s)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	i := 0
	for i < len(b) {
		switch {
		case b[i] == '-' && i+1 < len(b) && b[i+1] == '-':
			for i < len(b) && b[i] != '\n' {
				blank(i)
				i++
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			depth := 0
			for i < len(b) {
				if b[i] == '/' && i+1 < len(b) && b[i+1] == '*' {
					depth++
					blank(i)
					blank(i + 1)
					i += 2
					continue
				}
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					depth--
					blank(i)
					blank(i + 1)
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				blank(i)
				i++
			}
		case b[i] == '\'':
			blank(i)
			i++
			for i < len(b) {
				if b[i] == '\'' {
					blank(i)
					i++
					if i < len(b) && b[i] == '\'' { // '' escape — still inside
						blank(i)
						i++
						continue
					}
					break
				}
				blank(i)
				i++
			}
		case b[i] == '"':
			blank(i)
			i++
			for i < len(b) {
				if b[i] == '"' {
					blank(i)
					i++
					break
				}
				blank(i)
				i++
			}
		case b[i] == '$':
			if tag, ok := dollarTagAt(b, i); ok {
				end := strings.Index(string(b[i+len(tag):]), tag)
				stop := len(b)
				if end >= 0 {
					stop = i + len(tag) + end + len(tag)
				}
				for ; i < stop; i++ {
					blank(i)
				}
				continue
			}
			i++
		default:
			i++
		}
	}
	return string(out)
}

// dollarTagAt reports the dollar-quote tag starting at i ($$ or $name$), or
// false when the $ is something else (a positional parameter such as $1, or a
// bare dollar).
func dollarTagAt(b []byte, i int) (string, bool) {
	j := i + 1
	for j < len(b) && (b[j] == '_' || (b[j] >= 'a' && b[j] <= 'z') || (b[j] >= 'A' && b[j] <= 'Z')) {
		j++
	}
	if j < len(b) && b[j] == '$' {
		return string(b[i : j+1]), true
	}
	return "", false
}

// insertLedgerRow is the ONE place the ledger key is written. A second spelling
// of this statement is how the two halves of "applied" drift apart.
const insertLedgerRow = "INSERT INTO schema_migrations (filename) VALUES ($1)"

// applyMigration executes one planned migration and records it.
//
// The wrappable path is the whole point: BEGIN, the file's SQL, the ledger row,
// COMMIT. There is no instant at which the schema has moved and the ledger has
// not — the state that produced a permanent CrashLoopBackOff on 2026-08-07.
func applyMigration(p migrationPlan) error {
	if !p.Wrappable {
		// Stated at boot, every time, because this migration's resumability
		// rests on its own idempotence rather than on a transaction.
		log.Printf("Applying migration %s OUTSIDE a transaction: %s. Its ledger row commits separately, "+
			"so it must be idempotent to be resumable (asserted by TestNonWrappableMigrationsAreIdempotent)",
			p.File, p.NotWrappableBecause)
		if _, err := DB.Exec(p.Body); err != nil {
			return fmt.Errorf("exec %s: %w", p.File, err)
		}
		if migrationCrashAfterDDL != nil {
			if err := migrationCrashAfterDDL(p.File); err != nil {
				return err
			}
		}
		if _, err := DB.Exec(insertLedgerRow, p.File); err != nil {
			return fmt.Errorf("record migration %s: %w", p.File, err)
		}
		return nil
	}

	tx, err := DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction for %s: %w", p.File, err)
	}
	// Rollback on EVERY path that is not an explicit Commit, including a panic.
	// A committed transaction's Rollback is a no-op (sql.ErrTxDone), which is
	// why this is safe to defer unconditionally.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(p.Body); err != nil {
		return fmt.Errorf("exec %s: %w", p.File, err)
	}
	if migrationCrashAfterDDL != nil {
		if err := migrationCrashAfterDDL(p.File); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(insertLedgerRow, p.File); err != nil {
		return fmt.Errorf("record migration %s: %w", p.File, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", p.File, err)
	}
	return nil
}
