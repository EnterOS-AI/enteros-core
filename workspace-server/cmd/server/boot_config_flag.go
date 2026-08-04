package main

import (
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/envx"
)

// bootConfigEnabled reports whether CORE-served boot-config token delivery is
// active — the FINAL, platform-agnostic config path (no R2, no CP dependency).
//
// UNCONDITIONAL as of core#5047: shipped capability does not sit behind a
// default-off flag. MOLECULE_BOOT_CONFIG_ENABLE survives as an OPERATOR
// KILL-SWITCH only — set it to 0/false/no/off to go back to a deployment that
// mints no token and 404s the boot-config endpoint, without a redeploy.
// Unset, empty, or truthy all mean ON.
func bootConfigEnabled() bool {
	return envx.Enabled("MOLECULE_BOOT_CONFIG_ENABLE")
}
