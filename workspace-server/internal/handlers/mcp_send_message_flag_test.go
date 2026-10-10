package handlers

import (
	"os"
	"testing"
)

// unsetEnvForTest genuinely REMOVES the variable and restores it afterwards.
// t.Setenv(name, "") leaves it SET-but-empty, which is NOT the state a
// never-configured deployment is in. The UNSET case is the load-bearing one:
// MOLECULE_MCP_ALLOW_SEND_MESSAGE has never been set in any tenant container
// or in Infisical, so "unset" IS production.
func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	prev, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetenv %s: %v", name, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, prev)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

// send_message_to_user is shipped capability, not a dark feature. With NOTHING
// configured it must be available.
func TestMCPSendMessageEnabled_DefaultsOnWhenUnset(t *testing.T) {
	unsetEnvForTest(t, "MOLECULE_MCP_ALLOW_SEND_MESSAGE")
	if _, present := os.LookupEnv("MOLECULE_MCP_ALLOW_SEND_MESSAGE"); present {
		t.Fatal("precondition failed: MOLECULE_MCP_ALLOW_SEND_MESSAGE is still set")
	}
	if !mcpSendMessageEnabled() {
		t.Error("send_message_to_user must be enabled with the env var UNSET — it is shipped capability")
	}
}

// The variable survives as an operator kill-switch: no redeploy required to
// take the tool away again.
func TestMCPSendMessageEnabled_KillSwitchDisables(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "no", "off", " off "} {
		t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", v)
		if mcpSendMessageEnabled() {
			t.Errorf("MOLECULE_MCP_ALLOW_SEND_MESSAGE=%q must disable send_message_to_user", v)
		}
	}
}

// An explicit opt-in still works — operators with "true" already in their env
// must not be surprised.
func TestMCPSendMessageEnabled_ExplicitTrueStillEnables(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", v)
		if !mcpSendMessageEnabled() {
			t.Errorf("MOLECULE_MCP_ALLOW_SEND_MESSAGE=%q must keep send_message_to_user enabled", v)
		}
	}
}

// The tool LIST is the surface that made this dark for 15 weeks: the tool was
// absent from every MCP tools/list, so no agent could discover it. Assert on
// the list itself, not just the predicate — and assert the list is non-empty
// so the loop cannot pass vacuously.
func TestMCPToolList_IncludesSendMessage_WhenEnvUnset(t *testing.T) {
	unsetEnvForTest(t, "MOLECULE_MCP_ALLOW_SEND_MESSAGE")
	tools := mcpToolList(true)
	if len(tools) == 0 {
		t.Fatal("tool list is empty — the assertion below would pass vacuously")
	}
	found := false
	for _, tl := range tools {
		if tl.Name == "send_message_to_user" {
			found = true
		}
	}
	if !found {
		t.Errorf("send_message_to_user missing from tools/list with the env var UNSET (%d tools listed)", len(tools))
	}
}

func TestMCPToolList_OmitsSendMessage_WhenKillSwitchSet(t *testing.T) {
	t.Setenv("MOLECULE_MCP_ALLOW_SEND_MESSAGE", "off")
	tools := mcpToolList(true)
	if len(tools) == 0 {
		t.Fatal("tool list is empty — the assertion below would pass vacuously")
	}
	for _, tl := range tools {
		if tl.Name == "send_message_to_user" {
			t.Error("send_message_to_user must be omitted when the kill-switch is set")
		}
	}
}
