package handlers

import (
	"context"
	"net"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestResolveAgentURL_LoopbackRoutes(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "[::1]"} {
		for _, cached := range []bool{false, true} {
			for _, runtime := range []string{"hermes", "external"} {
				t.Run(host+"/"+runtime+map[bool]string{true: "/cached", false: "/db"}[cached], func(t *testing.T) {
					mock := setupTestDB(t)
					mr := setupTestRedis(t)
					defer setPlatformInDockerForTest(true)()
					// Routing is tested independently of real Docker DNS; the
					// dial-guard tests below exercise the actual SSRF policy.
					defer setSSRFCheckForTest(false)()
					h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
					waitForHandlerAsyncBeforeDBCleanup(t, h)
					h.provisioner = &stubLocalProv{}
					const id = "routing-test"
					advertised := "http://" + host + ":44911"
					if cached {
						mr.Set("ws:"+id+":url", advertised)
					} else {
						mock.ExpectQuery("SELECT url, status FROM workspaces").WithArgs(id).
							WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(advertised, "online"))
					}
					mock.ExpectQuery("SELECT COALESCE\\(runtime").WithArgs(id).
						WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow(runtime))
					got, err := h.resolveAgentURL(context.Background(), id)
					want := advertised
					if runtime != "external" {
						want = provisioner.InternalURL(id)
					}
					if err != nil || got != want {
						t.Fatalf("resolve = %q, %v; want %q", got, err, want)
					}
					mock.ExpectQuery("SELECT COALESCE\\(runtime").WithArgs(id).
						WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow(runtime))
					restartURL, restartErr := h.resolveAgentURLForRestartSignal(context.Background(), id)
					if restartErr != nil || restartURL != want {
						t.Fatalf("restart route = %q, %v; want %q", restartURL, restartErr, want)
					}
					if err := mock.ExpectationsWereMet(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestWorkspaceRoutes_RuntimeLookupFailure(t *testing.T) {
	for _, route := range []string{"chat", "restart"} {
		t.Run(route, func(t *testing.T) {
			mock := setupTestDB(t)
			mr := setupTestRedis(t)
			defer setPlatformInDockerForTest(true)()
			h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			waitForHandlerAsyncBeforeDBCleanup(t, h)
			h.provisioner = &stubLocalProv{}
			mr.Set("ws:unknown-runtime:url", "http://localhost:44911")
			mock.ExpectQuery("SELECT COALESCE\\(runtime").WithArgs("unknown-runtime").
				WillReturnError(context.DeadlineExceeded)
			if route == "chat" {
				got, err := h.resolveAgentURL(context.Background(), "unknown-runtime")
				if got != "" || err == nil || err.Status != 500 {
					t.Fatalf("must fail route lookup, got %q, %v", got, err)
				}
			} else {
				got, err := h.resolveAgentURLForRestartSignal(context.Background(), "unknown-runtime")
				if got != "" || err == nil {
					t.Fatalf("must fail route lookup, got %q, %v", got, err)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoopbackHTTPURL_RejectsLookalikes(t *testing.T) {
	for _, raw := range []string{
		"http://localhost.example:8000", "http://127.0.0.1.example:8000",
		"http://localhost@external.example:8000", "http://external.example@localhost:8000",
		"http://192.168.1.2:8000", "https://localhost:8000", "not a URL",
	} {
		if isLoopbackHTTPURL(raw) {
			t.Errorf("must not rewrite %q", raw)
		}
	}
}

func TestSafeDialControl_IPv6Policy(t *testing.T) {
	defer setSSRFCheckForTest(true)()
	oldLoopback := testAllowLoopback
	testAllowLoopback = false
	t.Cleanup(func() { testAllowLoopback = oldLoopback })
	t.Setenv("MOLECULE_ORG_ID", "")
	for _, env := range []string{"development", "production"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("MOLECULE_ENV", env)
			for _, ip := range []string{"::1", "fd00::1", "fe80::1", "::", "::ffff:169.254.169.254", "2606:4700:4700::1111"} {
				wantAllowed := ip == "2606:4700:4700::1111" || (env == "development" && (ip == "::1" || ip == "fd00::1"))
				err := safeDialControl("tcp6", net.JoinHostPort(ip, "8000"), nil)
				if (err == nil) != wantAllowed {
					t.Errorf("%s: allowed=%v, error=%v", ip, wantAllowed, err)
				}
			}
		})
	}
}

func TestSafeDialer_IPv6LoopbackInDevelopment(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	defer setSSRFCheckForTest(true)()
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	defer listener.Close()
	dialer := safeDialer()
	conn, err := dialer.DialContext(context.Background(), "tcp6", listener.Addr().String())
	if err != nil {
		t.Fatalf("development IPv6 loopback must connect: %v", err)
	}
	conn.Close()
}
