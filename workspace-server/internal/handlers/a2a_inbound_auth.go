package handlers

import (
	"context"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// inboundSecretReader is readOrLazyHealInboundSecret's signature, named so
// tests can substitute a fake and exercise every branch below without a
// database. Returns (secret, healed, err) where `healed` means the secret
// was minted during THIS call and the workspace has therefore not received
// it yet.
type inboundSecretReader func(ctx context.Context, workspaceID, opLabel string) (string, bool, error)

// resolveA2AInboundSecret decides which bearer, if any, an outbound A2A
// dispatch carries.
//
// WHY THIS IS NOT KEYED ON URL SHAPE ANY MORE
//
// The original rule (core#3319) attached the workspace's
// platform_inbound_secret only when isExternalAgentURL said the target was
// external. isExternalAgentURL is an SSRF classifier: it answers "is this
// host safe to dial", which is a different question from "must I prove who
// I am to it". Reusing one answer for both means the SAME agent is
// authenticated or not depending only on the string it was addressed by.
//
// Concretely, the address this platform is MOVING TO is the one that loses
// the credential: a per-workspace tunnel `ws-<id>.<appDomain>` matches
// isPlatformTunnelHostname and is classified internal, so a dispatch that
// egresses over the public internet is exactly the dispatch that sends
// nothing. (Measured on the live deployment: MOLECULE_APP_DOMAIN is unset,
// so the default `moleculesai.app` applies and every
// `ws-<id>.moleculesai.app` target takes the no-credential path.)
//
// So: send the bearer regardless of shape.
//
// TWO-STEP, ONE-WAY ROLLOUT
//
//	step 1 — MOLECULE_A2A_ALWAYS_AUTH on tenants (this function)
//	step 2 — MOLECULE_A2A_REQUIRE_AUTH on workspace runtimes
//
// Step 2 before step 1 returns 401 to every live agent. Hence the flag:
// step 1 is separately flippable and separately revertible, and a workspace
// ignores an Authorization header it does not yet check, so step 1 is inert
// on its own.
//
// BLAST RADIUS OF THE NEW BRANCH
//
// The external path keeps its pre-existing 503 contract byte for byte —
// those callers were already required to authenticate. The newly covered
// (previously internal) path is BEST-EFFORT and returns no error ever: it
// carries container-local traffic for agents that are serving right now, so
// a transient secret-read failure must not become a dispatch failure. When
// the secret cannot be read, or was only just minted (the workspace has not
// picked it up yet, so a bearer would be one it cannot match), the dispatch
// proceeds unauthenticated exactly as it did before this flag existed and
// self-heals on the workspace's next heartbeat.
func resolveA2AInboundSecret(
	ctx context.Context,
	workspaceID string,
	agentURL string,
	read inboundSecretReader,
	alwaysAuth bool,
) (string, *proxyA2AError) {
	if isExternalAgentURL(workspaceID, agentURL) {
		secret, healed, err := read(ctx, workspaceID, "ProxyA2A")
		if err != nil {
			log.Printf("ProxyA2A: no platform_inbound_secret for external workspace %s: %v", workspaceID, err)
			return "", &proxyA2AError{
				Status: http.StatusServiceUnavailable,
				Response: gin.H{
					"error":  "workspace not yet enrolled in inbound auth (RFC #2312)",
					"detail": "Failed to read platform_inbound_secret. Reprovision the workspace if this persists.",
				},
			}
		}
		if healed {
			return "", &proxyA2AError{
				Status: http.StatusServiceUnavailable,
				Response: gin.H{
					"error":               "workspace re-registering — please retry in 30 seconds",
					"detail":              "Inbound auth secret was just minted. Workspace will pick it up on its next heartbeat.",
					"retry_after_seconds": 30,
				},
			}
		}
		return secret, nil
	}

	if !alwaysAuth {
		// Pre-flag behaviour: internal addresses carry no credential.
		return "", nil
	}

	secret, healed, err := read(ctx, workspaceID, "ProxyA2A")
	switch {
	case err != nil:
		log.Printf("ProxyA2A: always-auth on, but no platform_inbound_secret for %s: %v — dispatching unauthenticated (pre-flag behaviour)", workspaceID, err)
		return "", nil
	case healed:
		log.Printf("ProxyA2A: always-auth on, secret just minted for %s — dispatching unauthenticated until the workspace picks it up", workspaceID)
		return "", nil
	default:
		return secret, nil
	}
}
