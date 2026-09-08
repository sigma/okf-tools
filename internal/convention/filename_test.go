package convention

import "testing"

// TestIsKebabFilename pins the naming policy OKF103 reports on and `okftool new`
// refuses to violate — one table, so the two can no longer disagree.
func TestIsKebabFilename(t *testing.T) {
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"graphrag.md", true},
		{"root-kek.md", true},
		{"a1-b2-c3.md", true},
		{"graphrag", true},
		{"", false},
		{".md", false},
		{"GraphRAG.md", false},
		{"graph_rag.md", false},
		{"graph rag.md", false},
		{"-leading.md", false},
		{"trailing-.md", false},
		{"double--hyphen.md", false},
		{"accént.md", false},
	} {
		if got := IsKebabFilename(tc.base); got != tc.want {
			t.Errorf("IsKebabFilename(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}
