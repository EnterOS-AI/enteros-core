//go:build integration
// +build integration

// light_enable_integration_test.go — enable and disable on real Postgres:
// disable puts the agent's own 24/7 setting (its MOLECULE_CONTINUOUS
// workspace secret) back exactly as it was before enable — absent stays
// absent, a value comes back byte for byte (review 21716, R3).
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"bytes"
	"database/sql"
	"net/http"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
)

type lightSecretOnDisk struct {
	present bool
	enc     []byte
	ver     int
}

func readWorkspaceSecretRow(t *testing.T, conn *sql.DB, id, key string) lightSecretOnDisk {
	t.Helper()
	var s lightSecretOnDisk
	err := conn.QueryRow(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`, id, key).Scan(&s.enc, &s.ver)
	if err == sql.ErrNoRows {
		return s
	}
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	s.present = true
	return s
}

func TestIntegration_LightDisable_Restores247SettingAsBeforeEnable(t *testing.T) {
	for _, prior := range []string{"", "false", "true"} {
		name := prior
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			conn := integrationDB_Light(t)
			setupTestRedis(t)
			seedLightWorkspaceWithID(t, conn, lightTestWS, "test-light-enable-247-"+name, "online", "")
			if prior != "" {
				enc, err := crypto.Encrypt([]byte(prior))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Exec(`INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version) VALUES ($1, 'MOLECULE_CONTINUOUS', $2, $3)`,
					lightTestWS, enc, crypto.CurrentEncryptionVersion()); err != nil {
					t.Fatal(err)
				}
			}
			before := readWorkspaceSecretRow(t, conn, lightTestWS, "MOLECULE_CONTINUOUS")

			lh, wh := newLightTestHandler(t, &fakeLightAPI{})
			if w := lightCall(lh.Enable, http.MethodPost, "/", "", nil); w.Code != http.StatusOK {
				t.Fatalf("enable = %d %s", w.Code, w.Body.String())
			}
			wh.waitAsyncForTest()
			during := readWorkspaceSecretRow(t, conn, lightTestWS, "MOLECULE_CONTINUOUS")
			plain, err := crypto.DecryptVersioned(during.enc, during.ver)
			if !during.present || err != nil || string(plain) != "false" {
				t.Fatalf("while Light: MOLECULE_CONTINUOUS present=%v value=%q (%v), want false", during.present, plain, err)
			}

			if w := lightCall(lh.Disable, http.MethodPost, "/", "", nil); w.Code != http.StatusOK {
				t.Fatalf("disable = %d %s", w.Code, w.Body.String())
			}
			wh.waitAsyncForTest()
			after := readWorkspaceSecretRow(t, conn, lightTestWS, "MOLECULE_CONTINUOUS")
			if after.present != before.present || !bytes.Equal(after.enc, before.enc) || after.ver != before.ver {
				t.Fatalf("after disable: present=%v ver=%d, want exactly the pre-enable row (present=%v ver=%d)", after.present, after.ver, before.present, before.ver)
			}
			if readWorkspaceSecretRow(t, conn, lightTestWS, "MOLECULE_LIGHT").present {
				t.Error("disable left MOLECULE_LIGHT behind")
			}
			var recorded sql.NullString
			if err := conn.QueryRow(`SELECT light_continuous_before::text FROM workspaces WHERE id = $1`, lightTestWS).Scan(&recorded); err != nil || recorded.Valid {
				t.Errorf("light_continuous_before after disable = %v (%v), want NULL", recorded, err)
			}
		})
	}
}
