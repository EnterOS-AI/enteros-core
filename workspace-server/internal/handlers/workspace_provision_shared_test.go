package handlers

// workspace_provision_shared_test.go — architectural test that pins
// the invariant the shared-prepare refactor relies on: every code
// path that provisions a workspace MUST call mintWorkspaceSecrets.
//
// Closes the drift class that produced the 2026-04-30 RFC #2312
// silent-503 bug. Pre-fix: provisionWorkspaceCP forgot to mint
// platform_inbound_secret because the SaaS path was implemented
// after the Docker path and the original mint call wasn't carried
// forward. Both modes now share mintWorkspaceSecrets via this
// extracted helper; this test ensures it stays that way.
//
// Same shape as the audit-coverage gate from #335 (#2343 PR-5).
// If this test fails: either add mintWorkspaceSecrets to the new
// provision function, OR (if the function legitimately should NOT
// mint) add it to provisionExemptFunctions with a one-line
// justification.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/provisioner"
)

// provisionFunctions are the set of functions that provision a
// workspace and therefore MUST call mintWorkspaceSecrets. Detected
// by name match (handler-method pattern); add a new entry whenever
// a new provision path is introduced.
var provisionFunctions = map[string]bool{
	"provisionWorkspace":     true,
	"provisionWorkspaceOpts": true,
	"provisionWorkspaceCP":   true,
}

// provisionExemptFunctions are functions whose name pattern matches
// the provision-function set but legitimately don't call
// mintWorkspaceSecrets (e.g., the wrapper provisionWorkspace which
// delegates to provisionWorkspaceOpts — the actual mint happens in
// the delegate, not the wrapper).
var provisionExemptFunctions = map[string]string{
	"provisionWorkspace": "thin wrapper that delegates to provisionWorkspaceOpts; the delegate mints",
}

// TestProvisionFunctions_AllCallMintWorkspaceSecrets asserts every
// non-exempt provision function in this package calls
// mintWorkspaceSecrets at least once in its body.
//
// The check is presence-in-function, not pairing-by-line — same
// rationale as the audit-coverage gate from #335.
func TestProvisionFunctions_AllCallMintWorkspaceSecrets(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	type missing struct {
		file string
		line int
		fn   string
	}
	var violations []missing

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if !provisionFunctions[fn.Name.Name] {
				continue
			}
			if _, exempt := provisionExemptFunctions[fn.Name.Name]; exempt {
				continue
			}
			if !callsMintWorkspaceSecrets(fn.Body) {
				violations = append(violations, missing{
					file: name,
					line: fset.Position(fn.Pos()).Line,
					fn:   fn.Name.Name,
				})
			}
		}
	}

	for _, v := range violations {
		t.Errorf(
			"%s:%d %s does not call mintWorkspaceSecrets — every provision path MUST mint auth_token + platform_inbound_secret. If audit is genuinely impossible here, add %q to provisionExemptFunctions in workspace_provision_shared_test.go with a justification.",
			v.file, v.line, v.fn, v.fn,
		)
	}
}

// callsMintWorkspaceSecrets walks the function body and reports
// whether mintWorkspaceSecrets is called anywhere — direct call OR
// via a helper. Recursion to helpers is shallow: we only check
// immediate calls in this function's body. The shared-prepare
// refactor centralizes mint in mintWorkspaceSecrets itself, so a
// direct call at the top-level is the expected pattern.
func callsMintWorkspaceSecrets(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "mintWorkspaceSecrets" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestMintWorkspaceSecrets_PersistsInboundSecretInSaaSMode is the
// behavioral counterpart to the AST gate. It pins the structural
// fix for the 2026-04-30 silent-503 chat upload bug (RFC #2312):
// even in SaaS mode (where Docker file injection is skipped),
// mintWorkspaceSecrets MUST persist platform_inbound_secret to the
// workspaces row so platform-side handlers can read it back.
//
// Pre-fix: provisionWorkspaceCP never called the mint helper, so
// every prod workspace had NULL platform_inbound_secret →
// chat_files Upload returned 503 with "workspace not yet enrolled
// in v2 upload" on every attempt.
func TestMintWorkspaceSecrets_PersistsInboundSecretInSaaSMode(t *testing.T) {
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")
	mock := setupTestDB(t)

	// First underlying call: revoke any existing live tokens. SaaS
	// mode early-returns from issueAndInjectToken right after this,
	// so IssueToken is NOT expected.
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at = now\(\) WHERE workspace_id = \$1 AND revoked_at IS NULL`).
		WithArgs("ws-saas-mint").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Second underlying call: persist the platform_inbound_secret.
	// The structural fix — without this UPDATE, the bug recurs.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-saas-mint").
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	cfg := provisioner.WorkspaceConfig{WorkspaceID: "ws-saas-mint"}
	handler.mintWorkspaceSecrets(context.Background(), "ws-saas-mint", &cfg)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met — mintWorkspaceSecrets did not persist platform_inbound_secret in SaaS mode (this is the prod bug recurrence): %v", err)
	}

	// Sanity: SaaS mode must NOT have written .platform_inbound_secret
	// into cfg.ConfigFiles — there's no Docker volume to deliver it.
	if _, present := cfg.ConfigFiles[".platform_inbound_secret"]; present {
		t.Errorf("SaaS mode should not inject .platform_inbound_secret into cfg.ConfigFiles (no Docker volume) — got entry")
	}
	if _, present := cfg.ConfigFiles[".auth_token"]; present {
		t.Errorf("SaaS mode should not inject .auth_token into cfg.ConfigFiles (no Docker volume) — got entry")
	}
}
