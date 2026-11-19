package staginge2e

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Model policy of the staging e2e lanes. Untagged on purpose: the pure parts
// below and their unit test (e2e_llm_model_test.go) run in ordinary CI, while
// the tagged live harnesses call them.
//
// Owner rule 2026-11-17: "use its cheapest model for any e2e testing". The deploy
// gate (staging-tenant-cd e2e-smoke: TestWorkspaceLifecycle_Staging and
// TestPlatformAgentMgmtMCP_Staging) used to run its concierge on the staging
// platform default, google/gemini-4-hydrogen, because the concierge is seeded
// with the platform default on every provision. The throwaway orgs are now
// created with llm_allowed_models=[the e2e model] (molecule-controlplane#3076):
// the CP enforces that allowlist on every proxied call, and serves the first
// allowlisted model as the org's MOLECULE_LLM_DEFAULT_MODEL, which the concierge
// reconcile seeds.

// e2eLLMModelDefault is the model the staging e2e lanes run on: Gemini 3.1
// Flash-Lite, the cheapest platform model. Never the platform default (Gemini 4
// Hydrogen, about six times the price) nor Gemini 4 Carbon (about eight).
const e2eLLMModelDefault = "google/gemini-3.1-flash-lite"

// e2eLLMModelFrom resolves the lane's model from an E2E_MODEL value ("" means
// the default) and says why it is refused, if it is: a platform-default-tier
// model may never be an e2e model, so a mis-set variable cannot move a lane
// back onto Hydrogen or Carbon.
func e2eLLMModelFrom(v string) (model, refusal string) {
	model = strings.TrimSpace(v)
	if model == "" {
		model = e2eLLMModelDefault
	}
	lower := strings.ToLower(model)
	for _, tier := range []string{"hydrogen", "carbon"} {
		if strings.Contains(lower, tier) {
			return model, fmt.Sprintf("%s is a platform-default-tier model (Gemini 4 %s); e2e lanes run on %s",
				model, strings.ToUpper(tier[:1])+tier[1:], e2eLLMModelDefault)
		}
	}
	return model, ""
}

// adminCreateOrgBody is the POST /cp/admin/orgs body for a throwaway e2e org.
// llmModels, when non-empty, restricts the org to those platform models
// (llm_allowed_models); a CP that predates that field answers 400, which the
// caller reports as such rather than retrying without it.
func adminCreateOrgBody(slug, name, provider string, llmModels []string) (string, error) {
	body := map[string]any{
		"slug":          slug,
		"name":          name,
		"owner_user_id": "e2e-runner:" + slug,
		"provider":      provider,
	}
	if len(llmModels) > 0 {
		body["llm_allowed_models"] = llmModels
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// servedThroughCIGateway reports whether an OpenAI chat completion returned for
// requested ("vendor/model") came from the CI LLM gateway rather than that
// vendor. The gateway rewrites any requested model and answers with its own
// served model ("huihui-ai/Qwen3-VL-30B-A3B-Instruct-abliterated" on
// 2026-11-18), never the vendor's, so a reply that names the vendor, or no
// model at all, is not the gateway. served is what the reply named.
func servedThroughCIGateway(requested, body string) (served string, ok bool) {
	var r struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(body), &r) != nil {
		return "", false
	}
	served = strings.TrimSpace(r.Model)
	vendor, _, _ := strings.Cut(requested, "/")
	if served == "" || (vendor != "" && strings.Contains(strings.ToLower(served), strings.ToLower(vendor))) {
		return served, false
	}
	return served, true
}

// workspaceModelFromBody reads GET /workspaces/:id/model ({"model","source"}).
func workspaceModelFromBody(body string) (model, source string) {
	var r struct {
		Model  string `json:"model"`
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(body), &r) != nil {
		return "", ""
	}
	return strings.TrimSpace(r.Model), r.Source
}
