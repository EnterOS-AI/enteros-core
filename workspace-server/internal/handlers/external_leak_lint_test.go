package handlers

// external_leak_lint_test.go — the CLASS-LEVEL regression guard for the
// "Connect your external agent" internal-address leak.
//
// The point of this file (vs. the field-specific e2e in
// external_connect_snippet_e2e_test.go) is to RED CI for the whole leak
// CLASS — including a NEW external endpoint added later — not just the one
// platform_url field that #1050 fixed.
//
// It enforces three structural/runtime invariants, none of which depends
// on enumerating today's endpoints:
//
//  1. SSOT-caller invariant (TestLeakLint_ExternalPayloadCallersUseSanitizedBase):
//     EVERY caller of BuildExternalConnectionPayload — present or future,
//     in any file — must source its base URL from externalPlatformURL()
//     (the single sanitized chokepoint), not from a raw c.Request.Host, a
//     hardcoded literal, or some other expression. A new external endpoint
//     that builds the connect payload from an unsanitized base reds CI
//     here, regardless of which file it lives in.
//
//  2. No internal-address literals in the connect-snippet builders
//     (TestLeakLint_ExternalBuildersNoInternalAddrLiteral): the dedicated
//     external_*.go response builders must not embed a docker-internal /
//     loopback URL authority in any string literal (comments are ignored —
//     the scan is AST-based, so prose discussing host.docker.internal is
//     fine). A new external_*.go endpoint that hardcodes an internal
//     address reds CI.
//
//  3. Sanitized chokepoint never leaks under the production env
//     (TestLeakLint_ExternalPlatformURLPrecedence): externalPlatformURL()
//     returns the public base whenever EXTERNAL_PLATFORM_URL or PLATFORM_URL
//     is set (the CP always sets at least one), falling through to the
//     request Host ONLY when neither is present.
//
// SCOPING — this guard is deliberately confined to molecule-core's
// EXTERNAL-facing handler package. The LEGITIMATE in-box host.docker.internal
// usage lives in molecule-controlplane (the local_docker provisioner and
// resolveTenantPlatformURL — correct for the in-box workspace→tenant path,
// where the workspace container shares the docker host with the tenant).
// Those are a different repo and package and are never examined here, so the
// guard cannot flag them.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// --- shared AST loading ---------------------------------------------------

// loadNonTestPackageFiles parses every non-test .go file in the current
// package directory (the test's cwd) and returns them with their FileSet.
func loadNonTestPackageFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files parsed — wrong cwd? leak-lint cannot run")
	}
	return fset, files
}

func isCallTo(expr ast.Expr, fn string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == fn
}

// --- Invariant 1: every connect-payload caller uses the sanitized base ----

const (
	payloadBuilderFn = "BuildExternalConnectionPayload"
	sanitizedBaseFn  = "externalPlatformURL"
)

func TestLeakLint_ExternalPayloadCallersUseSanitizedBase(t *testing.T) {
	fset, files := loadNonTestPackageFiles(t)

	sawAnyCall := false
	for name, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// Idents within THIS function assigned directly from
			// externalPlatformURL(...) — e.g. `platformURL := externalPlatformURL(c)`.
			sanitized := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i := 0; i < len(as.Lhs) && i < len(as.Rhs); i++ {
					if isCallTo(as.Rhs[i], sanitizedBaseFn) {
						if id, ok := as.Lhs[i].(*ast.Ident); ok {
							sanitized[id.Name] = true
						}
					}
				}
				return true
			})

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isCallTo(call, payloadBuilderFn) || len(call.Args) == 0 {
					return true
				}
				sawAnyCall = true
				base := call.Args[0]
				ok = isCallTo(base, sanitizedBaseFn)
				if !ok {
					if id, isID := base.(*ast.Ident); isID && sanitized[id.Name] {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s:%d: %s(...) is built from an UNSANITIZED base %q — every external-connection payload MUST derive its base URL from %s() (the single chokepoint that yields the public tunnel URL, never the internal request Host). This is the leak-class guard: route the base through %s().",
						name, fset.Position(call.Pos()).Line, payloadBuilderFn, exprString(base), sanitizedBaseFn, sanitizedBaseFn)
				}
				return true
			})
		}
	}
	if !sawAnyCall {
		t.Fatalf("found no %s call sites — the leak-lint lost its anchor (was the builder renamed?). Update payloadBuilderFn.", payloadBuilderFn)
	}
}

// exprString renders an expression compactly for a failure hint (no deps).
func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprString(v.Fun) + "(...)"
	case *ast.BasicLit:
		return v.Value
	default:
		return "<expr>"
	}
}

// --- Invariant 2: no internal-address literals in the connect builders ----

// loopbackAuthorityRe matches a loopback used as a URL authority (scheme://
// or user@ host), e.g. "http://localhost:3000", "https://127.0.0.1",
// "@[::1]". An operator-facing snippet that legitimately mentions a word in
// prose won't match — only an actual URL authority does.
var loopbackAuthorityRe = regexp.MustCompile(`(?i)(?:://|@)(?:127\.0\.0\.1|localhost|\[::1\]|::1)`)

func TestLeakLint_ExternalBuildersNoInternalAddrLiteral(t *testing.T) {
	fset, files := loadNonTestPackageFiles(t)

	scanned := 0
	for name, f := range files {
		// The dedicated connect-snippet response builders follow the
		// external_*.go convention (external_connection.go, external_rotate.go).
		// A new connect endpoint following the same convention is covered
		// automatically.
		if !strings.HasPrefix(name, "external_") {
			continue
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			low := strings.ToLower(s)
			// host.docker.internal and 0.0.0.0 are NEVER valid in a
			// customer-facing core response (the former is a docker-host-only
			// alias; the latter is a bind-all sentinel, not a routable host).
			for _, bad := range []string{"host.docker.internal", "0.0.0.0"} {
				if strings.Contains(low, bad) {
					t.Errorf("%s:%d: string literal embeds internal address %q — an external-facing builder must never emit it (unreachable off the docker host; topology leak).",
						name, fset.Position(lit.Pos()).Line, bad)
				}
			}
			if loc := loopbackAuthorityRe.FindString(s); loc != "" {
				t.Errorf("%s:%d: string literal embeds a loopback URL authority (%q) — a customer-facing connect snippet must point at the public platform URL, not a loopback.",
					name, fset.Position(lit.Pos()).Line, loc)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no external_*.go builder files scanned — the leak-lint lost its anchor (were they renamed?).")
	}
}

// --- Invariant 3: the sanitized chokepoint never leaks under prod env -----

func ctxWithInternalHost(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("GET", "/workspaces/ws/external/connection", nil)
	// The pre-#1050 leak: in the local-docker published-port posture the
	// request Host is host.docker.internal:<port>.
	req.Host = "host.docker.internal:50389"
	req.Header.Set("X-Forwarded-Proto", "https")
	c.Request = req
	return c
}

func TestLeakLint_ExternalPlatformURLPrecedence(t *testing.T) {
	const pub = "https://acme.moleculesai.app"

	// 1. EXTERNAL_PLATFORM_URL wins over everything (the CP-injected base).
	t.Run("external_platform_url_wins", func(t *testing.T) {
		t.Setenv("EXTERNAL_PLATFORM_URL", pub)
		t.Setenv("PLATFORM_URL", "https://wrong.example")
		if got := externalPlatformURL(ctxWithInternalHost(t)); got != pub {
			t.Errorf("got %q; want %q (EXTERNAL_PLATFORM_URL must win)", got, pub)
		}
	})

	// 2. Defense-in-depth: PLATFORM_URL wins over the request Host when
	//    EXTERNAL_PLATFORM_URL is absent (the EC2 prod posture, and the
	//    "deploy forgot EXTERNAL_PLATFORM_URL" case).
	t.Run("platform_url_fallback_beats_host", func(t *testing.T) {
		t.Setenv("EXTERNAL_PLATFORM_URL", "")
		t.Setenv("PLATFORM_URL", pub)
		got := externalPlatformURL(ctxWithInternalHost(t))
		if got != pub {
			t.Errorf("got %q; want %q (PLATFORM_URL fallback must beat the internal request Host)", got, pub)
		}
		if strings.Contains(got, "host.docker.internal") {
			t.Errorf("defense-in-depth failed: %q leaked the internal Host", got)
		}
	})

	// 3. Last resort: with NEITHER public-base env set, the request Host is
	//    used. This is the ONLY path that can leak — and it is closed in prod
	//    because the CP always sets at least one of the two envs (asserted in
	//    molecule-controlplane TestTenantPublicURLEnvArgs_*). Pinning it here
	//    locks the precedence: a regression that drops the env handling would
	//    flip cases 1-2 into this leak and red those tests.
	t.Run("host_is_last_resort_only", func(t *testing.T) {
		t.Setenv("EXTERNAL_PLATFORM_URL", "")
		t.Setenv("PLATFORM_URL", "")
		got := externalPlatformURL(ctxWithInternalHost(t))
		if got != "https://host.docker.internal:50389" {
			t.Errorf("got %q; want the request-Host fallback only when both envs are empty", got)
		}
	})
}
