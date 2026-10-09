package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// molecule-core#4997, "also fix the silence": Gitea answers 404 — not 403 — for
// a private repo the caller cannot see, so an unauthenticated fetch of a
// perfectly real private repo renders as "repository not found". That message
// cost this investigation days: it sends the reader hunting a typo in a repo
// that exists. The 404 cannot be reclassified (it is genuinely ambiguous), but
// whether WE presented a credential is not ambiguous, and saying so converts a
// dead end into a next step.

func TestGiteaArchive404_NoCredential_NamesTheMissingCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.NotFound(w, req)
	}))
	defer server.Close()

	err := defaultArchiveDownloader(context.Background(), server.URL+"/api/v1/repos/o/r/archive/main.tar.gz", "", t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("must still wrap ErrPluginNotFound (callers branch on it): %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "no credential was presented") {
		t.Errorf("404 with no credential must say so; got: %s", msg)
	}
	if !strings.Contains(msg, "may simply be PRIVATE") {
		t.Errorf("404 with no credential must name the private-repo possibility; got: %s", msg)
	}
}

func TestGiteaArchive404_WithCredential_SaysCredentialLacksAccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.NotFound(w, req)
	}))
	defer server.Close()

	err := defaultArchiveDownloader(context.Background(), server.URL+"/api/v1/repos/o/r/archive/main.tar.gz", "a-token", t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("must still wrap ErrPluginNotFound: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "the credential presented does not grant read") {
		t.Errorf("404 WITH a credential must distinguish itself; got: %s", msg)
	}
	if strings.Contains(msg, "a-token") {
		t.Fatalf("SECURITY: token value leaked into the error: %s", msg)
	}
}

func TestGiteaResolveSHA404_NoCredential_NamesTheMissingCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.NotFound(w, req)
	}))
	defer server.Close()

	r := &GiteaResolver{BaseURL: server.URL, ResolveRefClient: server.Client()}
	_, err := r.resolveSHA(context.Background(), "o", "r", "main", "", server.URL)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("must still wrap ErrPluginNotFound: %v", err)
	}
	if !strings.Contains(err.Error(), "no credential was presented") {
		t.Errorf("resolveSHA 404 with no credential must say so; got: %s", err.Error())
	}
}
