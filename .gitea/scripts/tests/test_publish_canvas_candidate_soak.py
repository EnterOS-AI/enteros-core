import re
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".gitea" / "workflows" / "publish-canvas-image.yml"
DIRECT_HELPERS = {
    ".gitea/scripts/infisical-read-secret.py",
    ".gitea/scripts/private-gitea-download.py",
    ".gitea/scripts/publish-canvas-selectors.sh",
    ".gitea/scripts/registry-manifest-digest.sh",
    ".gitea/scripts/registry-manifest-state.py",
}


def load_workflow() -> dict:
    return yaml.load(WORKFLOW.read_text(encoding="utf-8"), Loader=yaml.BaseLoader)


def run_bodies(job: dict) -> str:
    return "\n".join(step.get("run", "") for step in job.get("steps", []))


def step(job: dict, name: str) -> dict:
    return next(item for item in job["steps"] if item.get("name") == name)


def test_canvas_soak_is_push_main_candidate_publication_only() -> None:
    workflow = load_workflow()
    assert workflow["name"] == "publish-canvas-image"
    assert set(workflow["on"]) == {"push"}
    assert workflow["on"]["push"]["branches"] == ["main"]
    assert set(workflow["jobs"]) == {"build-and-push"}

    build = workflow["jobs"]["build-and-push"]
    assert build["name"] == "Build & push canvas image"
    assert build["continue-on-error"] == "true"
    assert "candidate_digest" in build["outputs"]
    assert workflow["permissions"] == {"contents": "read"}

    body = run_bodies(build)
    assert "staging-${GITHUB_SHA}" in body
    assert "staging-latest" in body
    assert "${IMAGE_NAME}:latest" not in body
    assert "prod-auto-deploy" not in body
    assert "prod-release-transaction" not in body
    assert "provider-promote" not in body


def test_external_actions_run_before_secret_acquisition() -> None:
    workflow = load_workflow()
    build = workflow["jobs"]["build-and-push"]
    checkout = step(build, "Checkout")
    assert checkout["with"]["ref"] == "${{ github.sha }}"
    assert checkout["with"]["fetch-depth"] == "0"
    assert checkout["with"]["persist-credentials"] == "false"

    secret_index = next(
        index
        for index, item in enumerate(build["steps"])
        if item.get("name") == "Fetch AUTO_SYNC_TOKEN from Infisical SSOT"
    )
    assert any(
        item.get("uses", "").startswith("docker/setup-buildx-action@")
        for item in build["steps"][:secret_index]
    )
    assert all("uses" not in item for item in build["steps"][secret_index:])


def test_canvas_soak_triggers_on_every_direct_checked_in_helper() -> None:
    workflow = load_workflow()
    build = workflow["jobs"]["build-and-push"]
    body = run_bodies(build)
    referenced_helpers = set(re.findall(r"\.gitea/scripts/[A-Za-z0-9_.-]+", body))
    assert referenced_helpers == DIRECT_HELPERS

    paths = set(workflow["on"]["push"]["paths"])
    assert "canvas/**" in paths
    assert ".gitea/workflows/publish-canvas-image.yml" in paths
    assert DIRECT_HELPERS <= paths


def test_canvas_candidate_is_write_once_bounded_and_digest_verified() -> None:
    workflow = load_workflow()
    build = workflow["jobs"]["build-and-push"]
    assert build["concurrency"] == {
        "group": "publish-canvas-image-build",
        "cancel-in-progress": "false",
    }
    assert re.fullmatch(r"[0-9a-f]{40}", workflow["env"]["OPERATOR_CONFIG_PUSHER_SHA"])
    assert re.fullmatch(
        r"[0-9a-f]{64}", workflow["env"]["OPERATOR_CONFIG_PUSHER_SHA256"]
    )

    checkout = step(build, "Checkout")
    assert checkout["with"]["ref"] == "${{ github.sha }}"
    reuse = step(build, "Reuse existing immutable Canvas candidate when present")
    publish = step(
        build, "Publish canvas image to registry (chunked pusher through CF tunnel)"
    )
    verify = step(build, "Repair and prove all Canvas candidate selectors")
    assert "registry-manifest-state.py" in reuse["run"]
    assert "revision" in reuse["run"] and "GITHUB_SHA" in reuse["run"]
    assert publish["if"] == "${{ steps.existing_candidate.outputs.reuse != 'true' }}"
    assert "private-gitea-download.py" in publish["run"]
    assert '"$PUSHER_SHA256" "$PUSHER"' in publish["run"]
    assert 'GITEA_DOWNLOAD_TOKEN="$AUTO_SYNC_TOKEN"' in publish["run"]
    assert "--token" not in publish["run"]
    assert publish["run"].index("registry-manifest-state.py --validate-base") < publish[
        "run"
    ].index('python3 "$PUSHER"')
    assert "--prefer-index=false" in publish["run"]
    assert "write-once" in publish["run"]
    assert verify.get("if") is None
    assert "publish-canvas-selectors.sh" in verify["run"]
    assert verify["env"]["EXPECTED_SHA"] == "${{ github.sha }}"
    assert verify["run"].index("registry-manifest-state.py --validate-base") < verify[
        "run"
    ].index('| docker login')


def test_secret_bearing_helpers_are_ssot_bounded_and_reject_redirects() -> None:
    infisical = (ROOT / ".gitea/scripts/infisical-read-secret.py").read_text()
    private_download = (ROOT / ".gitea/scripts/private-gitea-download.py").read_text()
    manifest_state = (ROOT / ".gitea/scripts/registry-manifest-state.py").read_text()
    manifest_digest = (ROOT / ".gitea/scripts/registry-manifest-digest.sh").read_text()

    assert "RejectRedirect" in infisical
    assert "MAX_JSON_BYTES" in infisical

    assert "RejectRedirect" in private_download
    assert 'parsed.hostname != "git.moleculesai.app"' in private_download
    assert "5 * 1024 * 1024 + 1" in private_download
    assert "os.replace" in private_download

    assert "RejectRedirect" in manifest_state
    assert 'CANONICAL_REGISTRY = "registry.moleculesai.app"' in manifest_state
    assert "Docker-Content-Digest" in manifest_state
    assert "MAX_MANIFEST_BYTES + 1" in manifest_state

    assert "REGISTRY_INSPECT_TIMEOUT_SECONDS" in manifest_digest
    assert "REGISTRY_MANIFEST_MAX_BYTES" in manifest_digest
    assert "sha256:" in manifest_digest


def test_active_ci_summary_describes_candidate_soak_without_promotion_claims() -> None:
    ci = (ROOT / ".gitea/workflows/ci.yml").read_text(encoding="utf-8")
    summary = ci.split("## Canvas image publication in progress", 1)[1].split(
        "BODY", 1
    )[0]
    assert "staging-<sha>" in summary
    assert "staging-latest" in summary
    assert "sha-<short-sha>" in summary
    assert "advisory" in summary.lower()
    assert "Promote `:latest`" not in summary
    assert "Wait for green main CI" not in summary
