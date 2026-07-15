"""Current API-contract ratchets for the minimal staging boot cell."""

from pathlib import Path
import shutil
import subprocess
import sys
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = (ROOT / "tests/e2e/test_minimal_boot_cell.sh").read_text()
WORKFLOW = (ROOT / ".gitea/workflows/boot-to-registration-e2e.yml").read_text()


class MinimalBootCellContractTest(unittest.TestCase):
    def test_org_create_uses_the_strict_current_payload(self) -> None:
        start = SCRIPT.index("PROVISION_PAYLOAD=")
        end = SCRIPT.index("PROVISION_HTTP_CODE=", start)
        create = SCRIPT[start:end]

        self.assertIn('"name": f"E2E {slug}"', create)
        self.assertIn('"owner_user_id": f"e2e-runner:{slug}"', create)
        self.assertIn('"provider": provider', create)
        for retired in ("runtime", "billing_mode", "model", "tier", "tags"):
            self.assertNotIn(f'"{retired}"', create)

    def test_registration_and_completion_use_tenant_workspace_routes(self) -> None:
        self.assertNotIn("/cp/registry/workspaces", SCRIPT)
        self.assertNotIn('"${CP_URL}/cp/rpc"', SCRIPT)
        self.assertIn("tenant_call GET /workspaces", SCRIPT)
        self.assertIn('curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$route"', SCRIPT)
        self.assertIn('"/workspaces/${WORKSPACE_ID}/a2a"', SCRIPT)
        self.assertIn('"/workspaces/${WORKSPACE_ID}/a2a/queue/${QUEUE_ID}"', SCRIPT)
        self.assertIn("a2a_assert_real_completion", SCRIPT)

    def test_teardown_confirms_and_verifies_org_removal(self) -> None:
        self.assertIn('-d "{\\"confirm\\":\\"${SLUG}\\"}"', SCRIPT)
        self.assertIn('TEARDOWN_STATUS="leak_risk_org_present"', SCRIPT)

    def test_workflow_runs_contract_ratchet_with_sufficient_timeout(self) -> None:
        self.assertIn("- 'tests/test_minimal_boot_cell_contract.py'", WORKFLOW)
        self.assertIn("python3 tests/test_minimal_boot_cell_contract.py", WORKFLOW)
        self.assertIn("timeout-minutes: 35", WORKFLOW)

    def test_workflow_name_does_not_claim_a_specific_ssot_runtime(self) -> None:
        self.assertNotIn("Minimal cell (claude-code", WORKFLOW)
        self.assertIn("Minimal cell (SSOT runtime", WORKFLOW)

    def test_tenant_readiness_and_workspace_detail_failures_are_diagnostic(self) -> None:
        self.assertIn(
            'TENANT_READY_TIMEOUT_SECS="${E2E_TENANT_READY_TIMEOUT_SECS:-300}"',
            SCRIPT,
        )
        self.assertIn('"$TENANT_URL/health"', SCRIPT)
        self.assertIn("WS_DETAIL_CODE=", SCRIPT)
        self.assertIn("detail_http=${WS_DETAIL_CODE:-000}", SCRIPT)
        self.assertIn('safe_body_preview "${WS_DETAIL:-}"', SCRIPT)

    def test_workspace_detail_parser_executes_and_preserves_fields(self) -> None:
        marker = 'eval "$(printf \'%s\' "$WS_DETAIL" | python3 -c \'\n'
        start = SCRIPT.index(marker) + len(marker)
        end = SCRIPT.index("\n' 2>/dev/null)\"", start)
        parser = SCRIPT[start:end]
        detail = (
            '{"status":"provisioning","url":"http://agent:8000",'
            '"last_heartbeat_at":"2026-07-15T06:57:13Z",'
            '"runtime":"hermes","model":"minimax/MiniMax-M2.7"}'
        )
        interpreters = [sys.executable]
        python311 = shutil.which("python3.11")
        if python311 and python311 not in interpreters:
            interpreters.append(python311)

        for interpreter in interpreters:
            with self.subTest(interpreter=interpreter):
                result = subprocess.run(
                    [interpreter, "-c", parser],
                    input=detail,
                    text=True,
                    capture_output=True,
                    check=False,
                )

                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("WS_STATUS=provisioning", result.stdout)
                self.assertIn("WS_RUNTIME=hermes", result.stdout)
                self.assertIn("WS_HEARTBEAT=2026-07-15T06:57:13Z", result.stdout)

    def test_transport_failures_produce_one_unambiguous_http_code(self) -> None:
        self.assertNotIn("|| echo 000", SCRIPT)
        for variable in (
            "PROVISION_HTTP_CODE",
            "TENANT_HEALTH_CODE",
            "WS_CODE",
            "WS_DETAIL_CODE",
            "A2A_CODE",
        ):
            self.assertIn(f"|| {variable}=000", SCRIPT)

    def test_gate_claims_only_authoritatively_observed_defaults(self) -> None:
        self.assertNotIn("BILLING_MODE=", SCRIPT)
        self.assertNotIn("WS_MODEL", SCRIPT)
        self.assertNotIn("MOLECULE_LLM_DEFAULT_MODEL", WORKFLOW)
        self.assertIn('[ -n "$WS_RUNTIME" ] ||', SCRIPT)
        self.assertIn('[ "$WS_RUNTIME" = "$RUNTIME" ] ||', SCRIPT)


if __name__ == "__main__":
    unittest.main()
