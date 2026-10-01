package gitx

import "testing"

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"package.json", "package.json", true},
		{"**/package.json", "packages/a/package.json", true},
		{"**/package.json", "package.json", true},
		{"requirements*.txt", "requirements-dev.txt", true},
		{"**/Cargo.toml", "crates/core/Cargo.toml", true},
		{"**/Cargo.toml", "Cargo.lock", false},
	}
	for _, tc := range cases {
		if got := MatchPath(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPath(%q,%q)=%t want %t", tc.pattern, tc.path, got, tc.want)
		}
	}
}
