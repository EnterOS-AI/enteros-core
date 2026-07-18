//go:build staging_e2e

package staginge2e

import (
	"regexp"
	"testing"
)

func TestE2ESlugIncludesExactCIRunIDScope(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "5679")

	slug := e2eSlug("req")
	if !regexp.MustCompile(`^e2e-req-5679-[0-9a-f]{6}$`).MatchString(slug) {
		t.Fatalf("e2eSlug() = %q, want exact run-scoped form", slug)
	}
}

func TestE2ESlugPreservesLocalFallback(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "")

	slug := e2eSlug("life")
	if !regexp.MustCompile(`^e2e-life-[0-9]{1,8}$`).MatchString(slug) {
		t.Fatalf("e2eSlug() = %q, want legacy local timestamp form", slug)
	}
}
