"""Current API-contract ratchets for the minimal staging boot cell."""

from pathlib import Path
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
        self.assertRegex(WORKFLOW, r"timeout-minutes:\s*(?:2[0-9]|[3-9][0-9])")


if __name__ == "__main__":
    unittest.main()
