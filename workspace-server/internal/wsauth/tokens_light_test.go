package wsauth

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// hashOf matches the bytea arg of a token INSERT: sha256 of the plaintext the
// call returned, captured after the fact.
type capturedHash struct{ got []byte }

func (c *capturedHash) Match(v driver.Value) bool {
	b, ok := v.([]byte)
	if ok {
		c.got = b
	}
	return ok && len(b) == sha256.Size
}

func TestIssueAPITokenWithIDReturnsTheRowID(t *testing.T) {
	db, mock := setupMock(t)
	hash := &capturedHash{}
	mock.ExpectQuery(`INSERT INTO workspace_auth_tokens \(workspace_id, token_hash, prefix, kind\)\s+VALUES \(\$1, \$2, \$3, 'api'\)\s+RETURNING id`).
		WithArgs("ws-abc", hash, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-row-1"))

	id, plaintext, err := IssueAPITokenWithID(context.Background(), db, "ws-abc")
	if err != nil {
		t.Fatalf("IssueAPITokenWithID: %v", err)
	}
	if id != "tok-row-1" {
		t.Errorf("id = %q, want the RETURNING id", id)
	}
	if want := sha256.Sum256([]byte(plaintext)); string(hash.got) != string(want[:]) {
		t.Errorf("stored hash is not sha256 of the returned plaintext")
	}
	if len(plaintext) < 40 {
		t.Errorf("plaintext too short for a 256-bit token: %d", len(plaintext))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestIssueAPITokenWithIDSurfacesAPersistFailure(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectQuery(`INSERT INTO workspace_auth_tokens`).WillReturnError(errors.New("boom"))
	if _, _, err := IssueAPITokenWithID(context.Background(), db, "ws-abc"); err == nil {
		t.Fatal("want an error")
	}
}

func TestRevokeTokenByIDIsScopedToTheWorkspace(t *testing.T) {
	db, mock := setupMock(t)
	mock.ExpectExec(`UPDATE workspace_auth_tokens\s+SET revoked_at = now\(\)\s+WHERE id = \$1 AND workspace_id = \$2 AND revoked_at IS NULL`).
		WithArgs("tok-1", "ws-abc").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("tok-1", "ws-abc").
		WillReturnResult(sqlmock.NewResult(0, 0))

	revoked, err := RevokeTokenByID(context.Background(), db, "ws-abc", "tok-1")
	if err != nil || !revoked {
		t.Fatalf("first revoke = %v, %v; want true", revoked, err)
	}
	revoked, err = RevokeTokenByID(context.Background(), db, "ws-abc", "tok-1")
	if err != nil || revoked {
		t.Fatalf("second revoke = %v, %v; want false (already revoked)", revoked, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
