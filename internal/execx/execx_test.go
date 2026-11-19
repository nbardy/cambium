package execx

import (
	"strings"
	"testing"
)

func TestChildEnvDropsRepositorySelectorsAndScrubs(t *testing.T) {
	parent := []string{"PATH=/bin", "GIT_DIR=/elsewhere/.git", "GIT_INDEX_FILE=/parent/index", "API_KEY=secret", "HOME=/h", "LC_ALL=C"}
	plain := strings.Join(childEnv(parent, map[string]string{"GIT_INDEX_FILE": "/mine"}, false), "\n")
	for _, want := range []string{"API_KEY=secret", "GIT_INDEX_FILE=/mine"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("%s missing from %s", want, plain)
		}
	}
	if strings.Contains(plain, "GIT_DIR") || strings.Contains(plain, "/parent/index") {
		t.Fatalf("repository selectors must not be inherited: %s", plain)
	}
	scrubbed := strings.Join(childEnv(parent, nil, true), "\n")
	if strings.Contains(scrubbed, "API_KEY") || !strings.Contains(scrubbed, "PATH=/bin") || !strings.Contains(scrubbed, "LC_ALL=C") || !strings.Contains(scrubbed, "core.hooksPath") {
		t.Fatalf("scrubbed environment wrong: %s", scrubbed)
	}
}
