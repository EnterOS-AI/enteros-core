//go:build staging_e2e

package staginge2e

import "testing"

func TestFirstChildOnPath(t *testing.T) {
	tests := []struct {
		name       string
		rows       []workspaceListRow
		ancestorID string
		descendant string
		want       string
		wantOK     bool
	}{
		{
			name: "direct child",
			rows: []workspaceListRow{
				{ID: "platform"},
				{ID: "ordinary", ParentID: "platform"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
			want:       "ordinary",
			wantOK:     true,
		},
		{
			name: "nested descendant returns first hop",
			rows: []workspaceListRow{
				{ID: "platform"},
				{ID: "former-root", ParentID: "platform"},
				{ID: "ordinary", ParentID: "former-root"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
			want:       "former-root",
			wantOK:     true,
		},
		{
			name: "descendant missing",
			rows: []workspaceListRow{
				{ID: "platform"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
		},
		{
			name: "ancestor missing",
			rows: []workspaceListRow{
				{ID: "ordinary", ParentID: "platform"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
		},
		{
			name: "different root",
			rows: []workspaceListRow{
				{ID: "platform"},
				{ID: "other-root"},
				{ID: "ordinary", ParentID: "other-root"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
		},
		{
			name: "cycle",
			rows: []workspaceListRow{
				{ID: "platform"},
				{ID: "ordinary", ParentID: "loop"},
				{ID: "loop", ParentID: "ordinary"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
		},
		{
			name: "duplicate id",
			rows: []workspaceListRow{
				{ID: "platform"},
				{ID: "ordinary", ParentID: "platform"},
				{ID: "ordinary", ParentID: "other"},
			},
			ancestorID: "platform",
			descendant: "ordinary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := firstChildOnPath(tt.rows, tt.ancestorID, tt.descendant)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("firstChildOnPath() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
