package plugins

import (
	"strings"
	"testing"
)

// validFullManifest is modelled on a real published manifest
// (molecule-ai-plugin-* shape): required trio + runtimes + kind +
// structured contributes.
const validFullManifest = `name: molecule-hitl
version: "1.2.0"
description: Human-in-the-loop approval gate for privileged tool calls.
author: Molecule AI
tags:
  - approval
  - hitl
kind: env-mutator
runtimes:
  - claude-code
  - crewai
  - hermes
skills:
  - approval-gate
contributes:
  skills:
    - id: approval-gate
      description: Pauses privileged tool calls for human approval.
  mcpServers:
    - name: molecule-hitl
      command: npx
      args: ["-y", "@molecule-ai/hitl-server"]
`

// joinViolations renders violations for failure messages.
func joinViolations(v []string) string { return strings.Join(v, "; ") }

// assertSomeViolationContains fails unless at least one violation message
// contains the given substring.
func assertSomeViolationContains(t *testing.T, violations []string, substr string) {
	t.Helper()
	if len(violations) == 0 {
		t.Fatalf("expected violations mentioning %q, got none", substr)
	}
	for _, v := range violations {
		if strings.Contains(v, substr) {
			return
		}
	}
	t.Errorf("expected a violation mentioning %q, got: %s", substr, joinViolations(violations))
}

func TestManifestSchema_Compiles(t *testing.T) {
	// Guards SDK contract asset corruption: the SSOT schema must be valid
	// JSON and compile as draft 2020-12.
	if _, err := compiledManifestSchema(); err != nil {
		t.Fatalf("SDK plugin-manifest schema failed to compile: %v", err)
	}
}

func TestValidateManifestSSOT_ValidFullManifest(t *testing.T) {
	if v := ValidateManifestSSOT([]byte(validFullManifest)); len(v) != 0 {
		t.Errorf("expected no violations for a conforming manifest, got: %s", joinViolations(v))
	}
}

func TestValidateManifestSSOT_MissingDescription(t *testing.T) {
	manifest := "name: my-plugin\nversion: \"1.0.0\"\n"
	assertSomeViolationContains(t, ValidateManifestSSOT([]byte(manifest)), "description")
}

func TestValidateManifestSSOT_PreReleaseVersionRejected(t *testing.T) {
	// The SSOT pattern ^[0-9]+(\.[0-9]+)*$ mirrors validate-plugin.py's
	// digits-and-dots-only check — "1.0-beta" must red.
	manifest := "name: my-plugin\nversion: \"1.0-beta\"\ndescription: d\n"
	assertSomeViolationContains(t, ValidateManifestSSOT([]byte(manifest)), "version")
}

func TestValidateManifestSSOT_RuntimesScalarRejected(t *testing.T) {
	// runtimes must be a list, not a bare string.
	manifest := "name: my-plugin\nversion: \"1.0.0\"\ndescription: d\nruntimes: claude-code\n"
	assertSomeViolationContains(t, ValidateManifestSSOT([]byte(manifest)), "runtimes")
}

func TestValidateManifestSSOT_UnknownRuntimeRejected(t *testing.T) {
	manifest := "name: my-plugin\nversion: \"1.0.0\"\ndescription: d\nruntimes:\n  - not-a-real-runtime\n"
	v := ValidateManifestSSOT([]byte(manifest))
	if len(v) == 0 {
		t.Fatal("expected an enum violation for runtimes: [not-a-real-runtime], got none")
	}
	assertSomeViolationContains(t, v, "runtimes")
}

func TestValidateManifestSSOT_GarbageBytesNotYAML(t *testing.T) {
	assertSomeViolationContains(t, ValidateManifestSSOT([]byte("{{{not yaml")), "not valid YAML")
}

func TestValidateManifestSSOT_TopLevelListRejected(t *testing.T) {
	// Non-object top level: the schema itself rejects it (type: object).
	manifest := "- name: my-plugin\n- version: \"1.0.0\"\n"
	if v := ValidateManifestSSOT([]byte(manifest)); len(v) == 0 {
		t.Error("expected a violation for a top-level YAML list, got none")
	}
}

func TestValidateManifestSSOT_LegacyUnderscoreRuntimeAliasAccepted(t *testing.T) {
	// claude_code is a legacy alias IN the SSOT enum — must NOT red
	// (today's published plugin.yaml files still use it).
	manifest := "name: my-plugin\nversion: \"1.0.0\"\ndescription: d\nruntimes:\n  - claude_code\n"
	if v := ValidateManifestSSOT([]byte(manifest)); len(v) != 0 {
		t.Errorf("legacy alias claude_code must be accepted, got: %s", joinViolations(v))
	}
}

func TestValidateManifestSSOT_UnknownTopLevelKeyTolerated(t *testing.T) {
	// Forward-compat MUST hold: the SSOT is additionalProperties:true, so a
	// newer manifest with an additive top-level key never reds here.
	manifest := "name: my-plugin\nversion: \"1.0.0\"\ndescription: d\nsomeFutureKey:\n  nested: true\n"
	if v := ValidateManifestSSOT([]byte(manifest)); len(v) != 0 {
		t.Errorf("unknown extra top-level key must be tolerated (additionalProperties:true), got: %s", joinViolations(v))
	}
}
