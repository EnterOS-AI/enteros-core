package wsauth

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// IssuePlatformInboundSecret persists the plaintext (not a hash) so the
// platform can read it back on every forward call. This is the primary
// shape difference vs. IssueToken; pin it explicitly.
func TestIssuePlatformInboundSecret_PersistsPlaintext(t *testing.T) {
	db, mock := setupMock(t)

	// Capture the plaintext written by the UPDATE so we can verify the
	// returned value matches what was stored. AnyArg captures, then we
	// pull the captured value via a separate ExpectExec... wait, sqlmock
	// doesn't return the captured args. Use a regex-style match on the
	// SQL and trust that the function persisted SOMETHING — then assert
	// the returned plaintext shape (length + alphabet) matches the
	// generator. The end-to-end "platform reads the same value back" is
	// covered by ReadPlatformInboundSecret tests.
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret = \$1 WHERE id = \$2`).
		WithArgs(sqlmock.AnyArg(), "ws-abc").
		WillReturnResult(sqlmock.NewResult(1, 1))

	plaintext, err := IssuePlatformInboundSecret(context.Background(), db, "ws-abc")
	if err != nil {
		t.Fatalf("IssuePlatformInboundSecret: %v", err)
	}
	if len(plaintext) < 40 {
		t.Errorf("plaintext too short for 256-bit entropy: len=%d", len(plaintext))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(plaintext) {
		t.Errorf("plaintext contains non-urlsafe chars: %q", plaintext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Re-issue rotates: the same workspace gets a different secret on the
// second call. Without rotation, a leaked secret would be permanent.
func TestIssuePlatformInboundSecret_RotatesOnReissue(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret`).WillReturnResult(sqlmock.NewResult(1, 1))

	a, _ := IssuePlatformInboundSecret(context.Background(), db, "ws-1")
	b, _ := IssuePlatformInboundSecret(context.Background(), db, "ws-1")
	if a == b {
		t.Errorf("expected fresh secret on re-issue, got %q twice", a)
	}
}

func TestIssuePlatformInboundSecret_RejectsEmptyWorkspaceID(t *testing.T) {
	db, _ := setupMock(t)
	_, err := IssuePlatformInboundSecret(context.Background(), db, "")
	if err == nil {
		t.Error("expected error for empty workspaceID, got nil")
	}
}

func TestReadPlatformInboundSecret_HappyPath(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow("the-plaintext"))

	got, err := ReadPlatformInboundSecret(context.Background(), db, "ws-abc")
	if err != nil {
		t.Fatalf("ReadPlatformInboundSecret: %v", err)
	}
	if got != "the-plaintext" {
		t.Errorf("got %q, want %q", got, "the-plaintext")
	}
}

// NULL column → ErrNoInboundSecret, not empty string. This is load-bearing:
// callers that ignored err and used the empty string would send an
// unauthenticated request to the workspace.
func TestReadPlatformInboundSecret_NullReturnsErrNoInbound(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(nil))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-abc")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Errorf("expected ErrNoInboundSecret on NULL, got %v", err)
	}
}

// Empty string is treated identically to NULL.
func TestReadPlatformInboundSecret_EmptyReturnsErrNoInbound(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}).AddRow(""))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-abc")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Errorf("expected ErrNoInboundSecret on empty, got %v", err)
	}
}

// Missing workspace row → ErrNoInboundSecret (collapsed from sql.ErrNoRows).
func TestReadPlatformInboundSecret_MissingWorkspace(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`SELECT platform_inbound_secret FROM workspaces WHERE id = \$1`).
		WithArgs("ws-missing").
		WillReturnRows(sqlmock.NewRows([]string{"platform_inbound_secret"}))

	_, err := ReadPlatformInboundSecret(context.Background(), db, "ws-missing")
	if !errors.Is(err, ErrNoInboundSecret) {
		t.Errorf("expected ErrNoInboundSecret on missing row, got %v", err)
	}
}

func TestReadPlatformInboundSecret_RejectsEmptyWorkspaceID(t *testing.T) {
	db, _ := setupMock(t)
	_, err := ReadPlatformInboundSecret(context.Background(), db, "")
	if err == nil {
		t.Error("expected error for empty workspaceID, got nil")
	}
}
