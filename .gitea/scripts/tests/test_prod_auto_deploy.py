import importlib.util
import sys
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "prod-auto-deploy.py"
spec = importlib.util.spec_from_file_location("prod_auto_deploy", SCRIPT)
prod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = prod
spec.loader.exec_module(prod)


def test_truthy_flag_accepts_operator_disable_values():
    for value in ("1", "true", "TRUE", "yes", "on", "disabled", "disable"):
        assert prod.truthy_flag(value) is True

    for value in ("", "0", "false", "no", "off", None):
        assert prod.truthy_flag(value) is False


def test_build_plan_defaults_to_staging_sha_target_and_prod_cp():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_DISABLED": "",
        }
    )

    assert plan["enabled"] is True
    assert plan["sha"] == "abcdef1234567890"
    assert plan["target_tag"] == "staging-abcdef1"
    assert plan["cp_url"] == "https://api.moleculesai.app"
    assert plan["body"] == {
        "target_tag": "staging-abcdef1",
        "canary_slug": "hongming",
        "soak_seconds": 60,
        "batch_size": 3,
        "dry_run": False,
        # cp#228 / task #308: fleet-wide intent must carry confirm:true.
        "confirm": True,
    }


def test_build_plan_always_sets_confirm_true_for_fleet_intent():
    """Regression guard: every plan body MUST carry confirm:true.

    CP /cp/admin/tenants/redeploy-fleet (cp#228) returns 400 on empty
    body / {confirm:false} / {only_slugs:[]} to prevent accidental
    fleet-wide mutation. This caller is fleet-wide intent (canary +
    fan-out, no slug scoping), so the plan MUST carry confirm:true.
    Pairs with cp#228's TestRedeployFleet_EmptyBodyReturns400 +
    TestRedeployFleet_ConfirmTrueProceeds.
    """
    plan = prod.build_plan({"GITHUB_SHA": "abcdef1234567890"})
    assert plan["body"]["confirm"] is True

    # Operator-overridable knobs do NOT drop the ack.
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_SOAK_SECONDS": "0",
            "PROD_AUTO_DEPLOY_BATCH_SIZE": "10",
            "PROD_AUTO_DEPLOY_DRY_RUN": "true",
            "PROD_AUTO_DEPLOY_CANARY_SLUG": "",
        }
    )
    assert plan["body"]["confirm"] is True


def test_build_plan_rejects_non_prod_cp_without_explicit_override():
    try:
        prod.build_plan(
            {
                "GITHUB_SHA": "abcdef1234567890",
                "CP_URL": "https://staging-api.moleculesai.app",
            }
        )
    except ValueError as exc:
        assert "PROD_ALLOW_NON_PROD_CP_URL=true" in str(exc)
    else:
        raise AssertionError("expected non-prod CP URL rejection")


def test_build_plan_allows_non_prod_cp_only_with_override():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "CP_URL": "https://staging-api.moleculesai.app",
            "PROD_ALLOW_NON_PROD_CP_URL": "true",
        }
    )

    assert plan["cp_url"] == "https://staging-api.moleculesai.app"


def test_build_plan_disable_flag_short_circuits_before_credentials():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_DISABLED": "true",
        }
    )

    assert plan["enabled"] is False
    assert plan["disabled_reason"] == "PROD_AUTO_DEPLOY_DISABLED=true"


def test_latest_status_for_context_uses_first_matching_status():
    statuses = [
        {"context": "CI / all-required (push)", "status": "pending"},
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "CI / all-required (push)", "status": "success"},
    ]

    latest = prod.latest_status_for_context(statuses, "CI / all-required (push)")

    assert latest == {"context": "CI / all-required (push)", "status": "pending"}


def test_ci_context_state_handles_missing_and_gitea_status_key():
    assert prod.ci_context_state([], "CI / all-required (push)") == "missing"
    assert (
        prod.ci_context_state(
            [{"context": "CI / all-required (push)", "status": "success"}],
            "CI / all-required (push)",
        )
        == "success"
    )
    assert (
        prod.ci_context_state(
            [{"context": "CI / all-required (push)", "state": "failure"}],
            "CI / all-required (push)",
        )
        == "failure"
    )


def test_context_is_satisfied_accepts_only_success():
    assert prod.context_is_satisfied("success") is True
    for state in ("failure", "error", "cancelled", "canceled", "skipped", "pending", "missing"):
        assert prod.context_is_satisfied(state) is False


def test_context_is_terminal_failure_rejects_cancelled_and_skipped():
    for state in ("failure", "error", "cancelled", "canceled", "skipped"):
        assert prod.context_is_terminal_failure(state) is True
    for state in ("pending", "missing", "success"):
        assert prod.context_is_terminal_failure(state) is False


def test_default_required_contexts_delegate_path_gating_to_all_required():
    assert prod.required_contexts({}) == [
        "CI / all-required (push)",
        "Secret scan / Scan diff for credential-shaped strings (push)",
    ]


def test_slugs_from_redeploy_response_uses_controlplane_plan_rows():
    body = {
        "results": [
            {"slug": "hongming", "phase": "canary", "ssm_status": "DryRun"},
            {"slug": "tenant-a", "phase": "batch-1", "ssm_status": "DryRun"},
            {"slug": "", "phase": "batch-1", "ssm_status": "DryRun"},
            {"phase": "batch-1", "ssm_status": "DryRun"},
        ]
    }

    assert prod.slugs_from_redeploy_response(body) == ["hongming", "tenant-a"]


def test_plan_rollout_slugs_asks_controlplane_for_dry_run_plan():
    calls = []

    def fake_redeploy(_cp_url, _token, body):
        calls.append(body)
        return 200, {
            "ok": True,
            "results": [
                {"slug": "hongming", "phase": "canary", "ssm_status": "DryRun"},
                {"slug": "tenant-a", "phase": "batch-1", "ssm_status": "DryRun"},
            ],
        }

    slugs = prod.plan_rollout_slugs(
        "https://api.moleculesai.app",
        "secret",
        {
            "target_tag": "staging-abcdef1",
            "canary_slug": "hongming",
            "soak_seconds": 60,
            "batch_size": 3,
            "dry_run": False,
            "confirm": True,
        },
        redeploy=fake_redeploy,
    )

    assert slugs == ["hongming", "tenant-a"]
    assert calls == [
        {
            "target_tag": "staging-abcdef1",
            "canary_slug": "hongming",
            "soak_seconds": 60,
            "batch_size": 3,
            "dry_run": True,
            "confirm": True,
        }
    ]


def test_scoped_redeploy_body_removes_canary_and_local_soak():
    base = {
        "target_tag": "staging-abcdef1",
        "canary_slug": "hongming",
        "soak_seconds": 60,
        "batch_size": 3,
        "dry_run": False,
        "confirm": True,
    }

    scoped = prod.scoped_redeploy_body(base, ["tenant-a", "tenant-b"])

    assert scoped == {
        "target_tag": "staging-abcdef1",
        "soak_seconds": 0,
        "batch_size": 2,
        "dry_run": False,
        "confirm": True,
        "only_slugs": ["tenant-a", "tenant-b"],
    }


def test_plan_scoped_rollout_preserves_canary_then_batches():
    calls, sleeps = [], []

    def fake_list(_cp_url, _token, _body):
        return ["tenant-a", "hongming", "tenant-b", "tenant-c"]

    def fake_redeploy(_cp_url, _token, body):
        calls.append(body)
        return 200, {
            "ok": True,
            "results": [{"slug": slug, "healthz_ok": True} for slug in body["only_slugs"]],
        }

    aggregate = prod.execute_scoped_rollout(
        {
            "cp_url": "https://api.moleculesai.app",
            "body": {
                "target_tag": "staging-abcdef1",
                "canary_slug": "hongming",
                "soak_seconds": 60,
                "batch_size": 2,
                "dry_run": False,
                "confirm": True,
            },
        },
        token="secret",
        list_slugs=fake_list,
        redeploy=fake_redeploy,
        sleep=sleeps.append,
    )

    assert [call["only_slugs"] for call in calls] == [
        ["hongming"],
        ["tenant-a", "tenant-b"],
        ["tenant-c"],
    ]
    assert sleeps == [60]
    assert aggregate["ok"] is True
    assert [result["slug"] for result in aggregate["results"]] == [
        "hongming",
        "tenant-a",
        "tenant-b",
        "tenant-c",
    ]


def test_scoped_rollout_halts_after_failed_canary():
    calls = []

    def fake_redeploy(_cp_url, _token, body):
        calls.append(body)
        return 200, {"ok": False, "results": [{"slug": body["only_slugs"][0], "error": "bad"}]}

    try:
        prod.execute_scoped_rollout(
            {
                "cp_url": "https://api.moleculesai.app",
                "body": {
                    "target_tag": "staging-abcdef1",
                    "canary_slug": "hongming",
                    "soak_seconds": 60,
                    "batch_size": 2,
                    "dry_run": False,
                    "confirm": True,
                },
            },
            token="secret",
            list_slugs=lambda _cp_url, _token, _body: ["hongming", "tenant-a"],
            redeploy=fake_redeploy,
            sleep=lambda _seconds: None,
        )
    except prod.RolloutFailed as exc:
        assert "redeploy scoped call failed" in str(exc)
        assert exc.response["ok"] is False
        assert exc.response["results"] == [{"slug": "hongming", "error": "bad"}]
    else:
        raise AssertionError("expected failed canary to halt rollout")

    assert [call["only_slugs"] for call in calls] == [["hongming"]]


def test_rollout_from_plan_file_writes_partial_response_on_failure(tmp_path):
    plan_path = tmp_path / "plan.json"
    response_path = tmp_path / "response.json"
    plan_path.write_text(
        """
        {
          "enabled": true,
          "cp_url": "https://api.moleculesai.app",
          "body": {"target_tag": "staging-abcdef1", "confirm": true}
        }
        """,
        encoding="utf-8",
    )

    original = prod.execute_scoped_rollout

    def fake_execute(_plan, _token):
        raise prod.RolloutFailed(
            "redeploy scoped call failed for hongming: HTTP 500, ok=false",
            {
                "ok": False,
                "error": "redeploy scoped call failed for hongming: HTTP 500, ok=false",
                "results": [{"slug": "hongming", "error": "bad"}],
            },
        )

    prod.execute_scoped_rollout = fake_execute
    try:
        try:
            prod.rollout_from_plan_file(
                str(plan_path),
                str(response_path),
                {"CP_ADMIN_API_TOKEN": "secret"},
            )
        except prod.RolloutFailed:
            pass
        else:
            raise AssertionError("expected rollout failure")
    finally:
        prod.execute_scoped_rollout = original

    assert response_path.read_text(encoding="utf-8").strip()
    assert '"ok": false' in response_path.read_text(encoding="utf-8")
    assert '"slug": "hongming"' in response_path.read_text(encoding="utf-8")
