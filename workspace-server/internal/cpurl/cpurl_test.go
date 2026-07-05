package cpurl

import "testing"

// TestBase_PreservesLegacyBehavior locks in that, with the OSS override
// (MOLECULE_CP_DEFAULT_URL) unset, Base() returns exactly what the previous
// inline fallbacks returned. This is the CP-safety guarantee: the managed
// SaaS and existing tenants must see byte-identical resolution.
func TestBase_PreservesLegacyBehavior(t *testing.T) {
	tests := []struct {
		name       string
		explicit   []string
		cpURL      string
		defaultURL string
		want       string
	}{
		{
			name: "no env, no explicit -> managed default (unchanged fallback)",
			want: ManagedDefault,
		},
		{
			name:  "MOLECULE_CP_URL wins over managed default",
			cpURL: "https://cp.example.com",
			want:  "https://cp.example.com",
		},
		{
			name:     "explicit override (CP_PROVISION_URL) wins over MOLECULE_CP_URL",
			explicit: []string{"https://provision.example.com"},
			cpURL:    "https://cp.example.com",
			want:     "https://provision.example.com",
		},
		{
			name:     "empty explicit is skipped, falls through to MOLECULE_CP_URL",
			explicit: []string{"", "  "},
			cpURL:    "https://cp.example.com",
			want:     "https://cp.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MOLECULE_CP_URL", tt.cpURL)
			t.Setenv("MOLECULE_CP_DEFAULT_URL", tt.defaultURL)
			if got := Base(tt.explicit...); got != tt.want {
				t.Fatalf("Base(%v) = %q, want %q", tt.explicit, got, tt.want)
			}
		})
	}
}

// TestBase_OSSOverride verifies the new seam: a deployment-wide default lets
// an OSS operator redirect the CP endpoint without code edits, while still
// ranking below the per-tenant MOLECULE_CP_URL and explicit overrides.
func TestBase_OSSOverride(t *testing.T) {
	t.Run("MOLECULE_CP_DEFAULT_URL used when MOLECULE_CP_URL unset", func(t *testing.T) {
		t.Setenv("MOLECULE_CP_URL", "")
		t.Setenv("MOLECULE_CP_DEFAULT_URL", "https://my-platform.internal")
		if got := Base(); got != "https://my-platform.internal" {
			t.Fatalf("Base() = %q, want OSS default", got)
		}
	})
	t.Run("MOLECULE_CP_URL still outranks the OSS default", func(t *testing.T) {
		t.Setenv("MOLECULE_CP_URL", "https://tenant-cp.internal")
		t.Setenv("MOLECULE_CP_DEFAULT_URL", "https://my-platform.internal")
		if got := Base(); got != "https://tenant-cp.internal" {
			t.Fatalf("Base() = %q, want per-tenant CP URL to win", got)
		}
	})
}
