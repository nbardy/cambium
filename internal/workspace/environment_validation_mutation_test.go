package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nbardy/cambium/internal/model"
)

// Optimizations for missing or unvalidated paths must retain the post-hook
// input check whenever a validator actually runs and could establish readiness.
func TestValidatorsThatChangeDeclaredInputsRemainUnverified(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell fixture requires sh")
	}
	for _, policy := range []string{"clone", "recreate"} {
		t.Run(policy, func(t *testing.T) {
			t.Setenv("CAMBIUM_TEST_MUTATE_INPUT", "")
			ctx := context.Background()
			repo := readinessRepository(t)
			writeReadinessFile(t, repo, "package.json", "{\"name\":\"fixture\"}\n")
			writeReadinessFile(t, repo, "package-lock.json", "original\n")
			writeReadinessFile(t, repo, "node_modules/state", "prepared\n")
			writeReadinessFile(t, repo, "validate-env.sh", "if [ -n \"$CAMBIUM_TEST_MUTATE_INPUT\" ]; then printf changed > package-lock.json; fi\n")
			prepare := ""
			if policy == "recreate" {
				prepare = "prepare = [\"sh\", \"-c\", \"mkdir -p node_modules && printf prepared > node_modules/state\"]\n"
			}
			writeReadinessFile(t, repo, ".cambium.toml", fmt.Sprintf(`version = 1
[[path]]
path = "node_modules"
policy = %q
inputs = ["package.json", "package-lock.json", "validate-env.sh"]
required = true
validate = ["sh", "validate-env.sh"]
%s`, policy, prepare))
			commitReadinessFiles(t, repo)
			manager := readinessManager(t, repo, true)
			first, err := manager.Create(ctx, CreateSpec{Name: "initial", Materializer: model.MaterializerGit})
			if err != nil || !first.EnvironmentReady {
				t.Fatalf("unchanged inputs did not validate: %#v, %v", first, err)
			}

			t.Setenv("CAMBIUM_TEST_MUTATE_INPUT", "1")
			second, err := manager.Create(ctx, CreateSpec{Name: "mutating", Materializer: model.MaterializerGit})
			if err != nil || second.EnvironmentReady || !slices.Contains(second.EnvironmentUnverified, "node_modules") {
				t.Fatalf("creation accepted inputs changed by its validator: %#v, %v", second, err)
			}
			assertFile(t, filepath.Join(second.Path, "package-lock.json"), "changed")
			assertFile(t, filepath.Join(repo, "package-lock.json"), "original\n")

			observation, err := manager.ValidateEnvironment(ctx, first.Name)
			if err != nil || observation.Ready || !slices.Contains(observation.Unverified, "node_modules") {
				t.Fatalf("explicit validation accepted its own changed inputs: %#v, %v", observation, err)
			}
			assertFile(t, filepath.Join(first.Path, "package-lock.json"), "changed")
		})
	}
}
