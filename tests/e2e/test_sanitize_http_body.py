"""Behavioral contracts for credential-safe E2E HTTP diagnostics."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import re
import subprocess
import sys
import unittest


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "tests" / "e2e" / "lib" / "redact_secrets.py"
OPS_WORKFLOW = ROOT / ".gitea" / "workflows" / "test-ops-scripts.yml"
E2E_SCRIPTS = (
    ROOT / "tests" / "e2e" / "test_staging_external_runtime.sh",
    ROOT / "tests" / "e2e" / "test_staging_full_saas.sh",
)
_RAW_RESPONSE_REFERENCE = re.compile(
    r"\$(?:\{)?(?P<name>(?:[A-Z][A-Z0-9_]*_RESP|REGISTER_BODY_JSON|PUT_BODY_OUT|AGENT_TEXT|DELEG_TEXT|EDIT_GET2?))(?![A-Z0-9_])(?::[^}]*)?\}?"
)


def _load_helper():
    assert HELPER.is_file(), "shared HTTP diagnostic sanitizer is missing"
    spec = importlib.util.spec_from_file_location("sanitize_http_body", HELPER)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _unsafe_raw_response_diagnostics(text: str) -> list[str]:
    """Return diagnostic lines whose raw response value bypasses sanitization."""

    violations = []
    for line_number, line in enumerate(text.splitlines(), 1):
        if not re.search(r"\b(?:log|fail|ok)\s", line):
            continue
        for reference in _RAW_RESPONSE_REFERENCE.finditer(line):
            if "sanitize_http_body" not in line[reference.end() :]:
                violations.append(f"line {line_number}: {reference.group('name')}")
    return violations


def _sanitizer_definition_precedes_calls(text: str) -> bool:
    definition = text.find("sanitize_http_body() {")
    calls = [
        match.start()
        for match in re.finditer(r"(?:\|\s*|\$\()sanitize_http_body\b", text)
    ]
    return definition >= 0 and bool(calls) and definition < min(calls)


class SanitizeHttpBodyTests(unittest.TestCase):
    def test_required_ops_workflow_executes_this_contract_once(self) -> None:
        workflow = OPS_WORKFLOW.read_text(encoding="utf-8")
        command = "python3 tests/e2e/test_sanitize_http_body.py"

        self.assertEqual(
            workflow.count(command),
            1,
            "the required Ops Scripts context must execute the sanitizer contract once",
        )

    def test_registration_json_redacts_secrets_but_keeps_proof_fields(self) -> None:
        sanitizer = _load_helper()
        payload = (
            '{"delivery_mode":"poll","status":"registered",'
            '"platform_inbound_secret":"synthetic-inbound-value",'
            '"auth_token":"synthetic-auth-value",'
            '"secret_digest":"synthetic-digest-value"}'
        )

        sanitized = sanitizer.redact(payload)

        self.assertNotIn("synthetic-inbound-value", sanitized)
        self.assertNotIn("synthetic-auth-value", sanitized)
        self.assertNotIn("synthetic-digest-value", sanitized)
        self.assertEqual(sanitized.count("<REDACTED>"), 3)
        self.assertIn('"delivery_mode":"poll"', sanitized)
        self.assertIn('"status":"registered"', sanitized)

    def test_header_and_form_credentials_are_redacted(self) -> None:
        sanitizer = _load_helper()
        payload = (
            "Authorization: Bearer synthetic-bearer-value\n"
            "platform_inbound_secret=synthetic-query-value&delivery_mode=poll"
        )

        sanitized = sanitizer.redact(payload)

        self.assertNotIn("synthetic-bearer-value", sanitized)
        self.assertNotIn("synthetic-query-value", sanitized)
        self.assertIn("Authorization: <REDACTED>", sanitized)
        self.assertIn(
            "platform_inbound_secret=<REDACTED>&delivery_mode=poll", sanitized
        )

    def test_complete_bearer_alphabet_is_redacted_without_a_suffix_leak(self) -> None:
        sanitizer = _load_helper()
        payload = '{"error":"upstream said Bearer alpha+LEAK~TAIL/= status=401"}'

        sanitized = sanitizer.redact(payload)

        self.assertNotIn("alpha+LEAK~TAIL/=", sanitized)
        self.assertNotIn("+LEAK~TAIL", sanitized)
        self.assertIn("Bearer <REDACTED> status=401", sanitized)

    def test_live_e2e_scripts_use_the_shared_sanitizer(self) -> None:
        expected_call = (
            'python3 "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)'
            '/lib/redact_secrets.py" --max-chars 4000 '
            "|| printf '%s' '<redaction failed>'"
        )

        for script in E2E_SCRIPTS:
            text = script.read_text(encoding="utf-8")
            self.assertIn(
                expected_call, text, f"{script.name} bypasses shared sanitization"
            )
            self.assertTrue(
                _sanitizer_definition_precedes_calls(text),
                f"{script.name} calls the sanitizer before Bash defines it",
            )
            self.assertEqual(
                _unsafe_raw_response_diagnostics(text),
                [],
                f"{script.name} logs an unsanitized HTTP response",
            )

    def test_raw_response_diagnostic_detector_fails_closed(self) -> None:
        unsafe = 'log "request failed: $REGISTER_RESP"'
        unsafe_fallback = (
            'fail "$(printf \'%s\' "$REGISTER_RESP" | sanitize_http_body '
            '|| echo "$REGISTER_RESP")"'
        )
        safe = (
            'fail "$(printf \'%s\' "$REGISTER_RESP" | sanitize_http_body '
            "|| printf '%s' '<redaction failed>')\""
        )

        self.assertEqual(
            _unsafe_raw_response_diagnostics(unsafe), ["line 1: REGISTER_RESP"]
        )
        self.assertEqual(
            _unsafe_raw_response_diagnostics(unsafe_fallback),
            ["line 1: REGISTER_RESP"],
        )
        self.assertEqual(_unsafe_raw_response_diagnostics(safe), [])

    def test_sanitizer_definition_order_check_fails_closed(self) -> None:
        call = 'log "$(printf \'%s\' "$REGISTER_RESP" | sanitize_http_body)"'
        definition = "sanitize_http_body() { printf '<redacted>'; }"

        self.assertFalse(_sanitizer_definition_precedes_calls(f"{call}\n{definition}"))
        self.assertTrue(_sanitizer_definition_precedes_calls(f"{definition}\n{call}"))

    def test_cli_truncates_only_after_redaction(self) -> None:
        result = subprocess.run(
            [sys.executable, str(HELPER), "--max-chars", "32"],
            input="token=synthetic-cli-value&context=" + ("x" * 100),
            text=True,
            capture_output=True,
            check=True,
        )

        self.assertEqual(len(result.stdout), 32)
        self.assertNotIn("synthetic-cli-value", result.stdout)
        self.assertTrue(result.stdout.startswith("token=<REDACTED>&context="))


if __name__ == "__main__":
    unittest.main()
