"""Ratchets for provider-neutral comments in current execution paths."""

from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def read(relative: str) -> str:
    return (ROOT / relative).read_text()


def assert_absent(text: str, *stale_claims: str) -> None:
    for claim in stale_claims:
        assert claim not in text, f"stale provider claim returned: {claim!r}"


def test_generic_workspace_provisioning_does_not_claim_ec2() -> None:
    provision = read("workspace-server/internal/handlers/workspace_provision.go")
    workspace = read("workspace-server/internal/handlers/workspace.go")
    org = read("workspace-server/internal/handlers/org.go")

    assert_absent(
        provision,
        "workspace → EC2",
        "live EC2",
        "EC2 instance %s is RUNNING but UNTRACKED",
        "EC2 untracked",
    )
    assert_absent(
        workspace,
        "every hosted workspace gets its own sibling\n\t\t// EC2 instance",
        "no\n\t// EC2 launched",
        "Async EC2 provisioning may still",
    )
    assert_absent(
        org,
        "running on EC2",
        "AWS RunInstances",
        "SaaS / EC2 backend",
    )


def test_reactive_health_docs_are_single_and_provider_neutral() -> None:
    text = read("workspace-server/internal/handlers/a2a_proxy_helpers.go")

    assert text.count(
        "maybeMarkContainerDead runs the reactive health check after a forward error."
    ) == 1
    assert_absent(
        text,
        "SaaS / EC2 deployment; IsRunning calls CP's",
        "dead EC2 instances still recover",
    )


def test_plugin_instance_id_docs_match_aws_shaped_fallback_guard() -> None:
    text = read("workspace-server/internal/handlers/plugins.go")

    assert_absent(
        text,
        "InstanceIDLookup resolves a workspace's EC2 instance_id",
        "instance_id → EC2 instance_id",
        "dispatch to the EIC SSH path\n// for SaaS workspaces",
    )
    assert "AWS-shaped" in text


def test_staging_tabs_docs_match_current_provider_default() -> None:
    text = read("canvas/e2e/staging-tabs.spec.ts")

    assert_absent(
        text,
        "remote EC2",
        "real staging EC2 tenant",
        "12-20 min cold boot",
        "AWS/Cloudflare/CP",
    )
    assert "molecules-server" in text


def test_legacy_provision_event_names_are_labeled_not_reinterpreted() -> None:
    text = read("workspace-server/internal/provlog/provlog.go")

    assert "Legacy event names are retained for compatibility" in text
    assert "provider compute" in text
    assert_absent(
        text,
        "provisioning-lifecycle boundaries (workspace create, EC2 start/stop",
        "workspace row inserted, EC2 about to launch",
        "idempotency hit, no new EC2",
        "RunInstances returned an instance id",
        "TerminateInstances acknowledged",
    )
