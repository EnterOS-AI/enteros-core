package provisioner

import (
	"testing"
	"time"
)

// TestCPProvisionCeilingIsTheProvisionClientBudget ties the exported ceiling to
// the client it claims to describe.
//
// Without this, handlers' "the provision context is at least
// CPProvisionCeiling()" assertion would be self-referential: both sides could
// agree on a number that no HTTP client actually uses, and the budget inversion
// would be back with a green suite over it. Constructing through
// NewCPProvisioner — the production path — is what makes the tie real.
func TestCPProvisionCeilingIsTheProvisionClientBudget(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-1")
	t.Setenv("CP_PROVISION_SHARED_SECRET", "s3cret")

	p, err := NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}
	if p.provisionHTTPClient == nil {
		t.Fatal("provision client not constructed")
	}
	if got := CPProvisionCeiling(); got != p.provisionHTTPClient.Timeout {
		t.Errorf("CPProvisionCeiling() = %s but provisionHTTPClient.Timeout = %s — "+
			"a caller sizing its context off the exported value would be sizing it off a number "+
			"nothing enforces", got, p.provisionHTTPClient.Timeout)
	}
	// The same floor core#5019's test pins on the client, restated on the
	// exported view so a caller reading only this symbol still gets the
	// guarantee.
	if CPProvisionCeiling() < 10*time.Minute {
		t.Errorf("CPProvisionCeiling() = %s, want >= 10m to cover a cold multi-GB image pull", CPProvisionCeiling())
	}
}
