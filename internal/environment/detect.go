package environment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/gitx"
)

// Builtins are conservative filesystem policies, not package-manager
// implementations. They describe common workspace-local outputs for popular
// language ecosystems. Git still owns every tracked file, and the actual
// package managers remain opaque commands running in ordinary worktrees.
func Builtins() []config.LayerRule {
	jsDeps := []string{
		"package.json", "**/package.json", "package-lock.json", "npm-shrinkwrap.json",
		"yarn.lock", "pnpm-lock.yaml", "pnpm-workspace.yaml", "bun.lock", "bun.lockb",
		".npmrc", ".yarnrc", ".yarnrc.yml", "yarn.config.cjs",
		".node-version", ".nvmrc", ".tool-versions", "mise.toml",
	}
	jsBuild := mergeStrings(jsDeps, []string{
		"tsconfig.json", "tsconfig*.json", "**/tsconfig.json", "**/tsconfig*.json",
		"turbo.json", "nx.json", "lerna.json", "vite.config.*", "webpack.config.*",
		"next.config.*", "nuxt.config.*", "svelte.config.*",
	})
	pythonDeps := []string{
		"pyproject.toml", "**/pyproject.toml", "uv.lock", "poetry.lock", "pdm.lock",
		"Pipfile", "Pipfile.lock", "requirements.txt", "requirements*.txt", "requirements*.in",
		"pylock.toml", "setup.py", "setup.cfg", "poetry.toml", "pdm.toml",
		".python-version", ".tool-versions", "mise.toml",
	}
	rustInputs := []string{
		"Cargo.toml", "**/Cargo.toml", "Cargo.lock", ".cargo/config", ".cargo/config.toml",
		"rust-toolchain", "rust-toolchain.toml", ".tool-versions", "mise.toml",
	}
	clojureInputs := []string{
		"deps.edn", "project.clj", "shadow-cljs.edn", "bb.edn", "figwheel-main.edn",
		".tool-versions", "mise.toml",
	}
	jvmInputs := []string{
		"pom.xml", "**/pom.xml", "build.gradle", "build.gradle.kts", "**/build.gradle", "**/build.gradle.kts",
		"settings.gradle", "settings.gradle.kts", "gradle.properties", "gradle/libs.versions.toml",
		"gradle/wrapper/gradle-wrapper.properties", ".java-version", ".sdkmanrc", ".tool-versions", "mise.toml",
	}
	goInputs := []string{"go.mod", "go.sum", "go.work", "go.work.sum", ".go-version", ".tool-versions", "mise.toml"}
	rubyPHPInputs := []string{
		"Gemfile", "Gemfile.lock", "gems.rb", "gems.locked", "composer.json", "composer.lock",
		".ruby-version", ".tool-versions", "mise.toml",
	}
	elixirInputs := []string{"mix.exs", "mix.lock", ".tool-versions", "mise.toml"}
	dartInputs := []string{"pubspec.yaml", "pubspec.lock", ".tool-versions", "mise.toml"}
	dotnetInputs := []string{
		"*.sln", "*.slnx", "*.csproj", "**/*.csproj", "*.fsproj", "**/*.fsproj",
		"Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props", "packages.lock.json", "**/packages.lock.json",
		"global.json",
	}
	swiftInputs := []string{"Package.swift", "Package.resolved", "Podfile", "Podfile.lock", "Cartfile", "Cartfile.resolved"}
	cppInputs := []string{"CMakeLists.txt", "**/CMakeLists.txt", "conanfile.txt", "conanfile.py", "conan.lock", "vcpkg.json", "vcpkg-configuration.json"}
	allBuildInputs := mergeStrings(jsBuild, pythonDeps, rustInputs, clojureInputs, jvmInputs, goInputs, rubyPHPInputs, elixirInputs, dartInputs, dotnetInputs, swiftInputs, cppInputs)

	rules := []config.LayerRule{
		// JavaScript, TypeScript, and Node-backed ClojureScript.
		{Name: "Node dependency tree", Path: "node_modules", Mode: config.LayerClone, Inputs: jsDeps, ActivateOnInputs: true},
		{Name: "Yarn local cache", Path: ".yarn/cache", Mode: config.LayerClone, Inputs: jsDeps},
		{Name: "Yarn unplugged tree", Path: ".yarn/unplugged", Mode: config.LayerClone, Inputs: jsDeps},
		{Name: "Yarn PnP map", Path: ".pnp.cjs", Mode: config.LayerClone, Inputs: jsDeps},
		{Name: "Yarn PnP loader", Path: ".pnp.loader.mjs", Mode: config.LayerClone, Inputs: jsDeps},
		{Name: "Yarn install state", Path: ".yarn/install-state.gz", Mode: config.LayerSeed, Inputs: jsDeps, SourceSensitive: true},
		{Name: "Yarn editor SDKs", Path: ".yarn/sdks", Mode: config.LayerSeed, Inputs: jsDeps, SourceSensitive: true},
		{Name: "project-local pnpm store", Path: ".pnpm-store", Mode: config.LayerClone, Inputs: jsDeps},
		{Name: "Next.js build", Path: ".next", Mode: config.LayerSeed, Inputs: jsBuild, SourceSensitive: true},
		{Name: "Nuxt build", Path: ".nuxt", Mode: config.LayerSeed, Inputs: jsBuild, SourceSensitive: true},
		{Name: "SvelteKit build", Path: ".svelte-kit", Mode: config.LayerSeed, Inputs: jsBuild, SourceSensitive: true},
		{Name: "Vite cache", Path: ".vite", Mode: config.LayerSeed, Inputs: jsBuild, SourceSensitive: true},
		{Name: "Turborepo cache", Path: ".turbo", Mode: config.LayerSeed, Inputs: jsBuild, SourceSensitive: true},

		// Python. Virtual environments are recreated because they commonly embed
		// absolute interpreter paths and shebangs.
		{Name: "Python virtual environment", Path: ".venv", Mode: config.LayerRecreate, Inputs: pythonDeps, ActivateOnInputs: true},
		{Name: "Python virtual environment", Path: "venv", Mode: config.LayerRecreate, Inputs: pythonDeps, ActivateOnInputs: true},
		{Name: "tox environments", Path: ".tox", Mode: config.LayerRecreate, Inputs: pythonDeps},
		{Name: "nox environments", Path: ".nox", Mode: config.LayerRecreate, Inputs: pythonDeps},
		{Name: "Pixi environment", Path: ".pixi", Mode: config.LayerRecreate, Inputs: pythonDeps},
		{Name: "Conda environment", Path: ".conda", Mode: config.LayerRecreate, Inputs: pythonDeps},
		{Name: "Python bytecode", Path: "__pycache__", Mode: config.LayerEmpty, Inputs: pythonDeps},
		{Name: "pytest cache", Path: ".pytest_cache", Mode: config.LayerSeed, Inputs: pythonDeps, SourceSensitive: true},
		{Name: "mypy cache", Path: ".mypy_cache", Mode: config.LayerSeed, Inputs: pythonDeps, SourceSensitive: true},
		{Name: "ruff cache", Path: ".ruff_cache", Mode: config.LayerSeed, Inputs: pythonDeps, SourceSensitive: true},
		{Name: "pyright cache", Path: ".pyright", Mode: config.LayerSeed, Inputs: pythonDeps, SourceSensitive: true},

		// Rust, Clojure, ClojureScript, Java, and Kotlin.
		{Name: "Rust, Clojure, or JVM build output", Path: "target", Mode: config.LayerSeed, Inputs: mergeStrings(rustInputs, clojureInputs, jvmInputs), SourceSensitive: true},
		{Name: "Clojure classpath cache", Path: ".cpcache", Mode: config.LayerSeed, Inputs: clojureInputs, SourceSensitive: true},
		{Name: "Clojure LSP cache", Path: ".lsp", Mode: config.LayerSeed, Inputs: clojureInputs, SourceSensitive: true},
		{Name: "clj-kondo cache", Path: ".clj-kondo/.cache", Mode: config.LayerSeed, Inputs: clojureInputs, SourceSensitive: true},
		{Name: "shadow-cljs cache", Path: ".shadow-cljs", Mode: config.LayerSeed, Inputs: mergeStrings(jsDeps, clojureInputs), SourceSensitive: true},
		{Name: "Figwheel cache", Path: ".figwheel-main", Mode: config.LayerSeed, Inputs: mergeStrings(jsDeps, clojureInputs), SourceSensitive: true},
		{Name: "Gradle project cache", Path: ".gradle", Mode: config.LayerSeed, Inputs: jvmInputs, SourceSensitive: true},

		// Go, Ruby, PHP, Elixir, Dart/Flutter, .NET, Swift, and C/C++.
		{Name: "vendored dependencies", Path: "vendor", Mode: config.LayerClone, Inputs: mergeStrings(goInputs, rubyPHPInputs)},
		{Name: "Elixir dependencies", Path: "deps", Mode: config.LayerClone, Inputs: elixirInputs},
		{Name: "Elixir build output", Path: "_build", Mode: config.LayerSeed, Inputs: elixirInputs, SourceSensitive: true},
		{Name: "Dart tool state", Path: ".dart_tool", Mode: config.LayerSeed, Inputs: dartInputs, SourceSensitive: true},
		{Name: ".NET intermediate output", Path: "obj", Mode: config.LayerSeed, Inputs: dotnetInputs, SourceSensitive: true},
		{Name: ".NET binary output", Path: "bin", Mode: config.LayerSeed, Inputs: dotnetInputs, SourceSensitive: true},
		{Name: "SwiftPM build output", Path: ".build", Mode: config.LayerSeed, Inputs: swiftInputs, SourceSensitive: true},
		{Name: "CocoaPods dependencies", Path: "Pods", Mode: config.LayerClone, Inputs: swiftInputs},
		{Name: "vcpkg installed tree", Path: "vcpkg_installed", Mode: config.LayerClone, Inputs: cppInputs},

		// Deliberately generic output names. They activate only when the path
		// exists in the source workspace or a compatible prepared layer exists.
		{Name: "generic distribution output", Path: "dist", Mode: config.LayerSeed, Inputs: allBuildInputs, SourceSensitive: true},
		{Name: "generic build output", Path: "build", Mode: config.LayerSeed, Inputs: allBuildInputs, SourceSensitive: true},
		{Name: "generic coverage output", Path: "coverage", Mode: config.LayerSeed, Inputs: allBuildInputs, SourceSensitive: true},
		{Name: "generic compiler output", Path: "out", Mode: config.LayerSeed, Inputs: allBuildInputs, SourceSensitive: true},
		{Name: "Ruby bundle metadata", Path: ".bundle", Mode: config.LayerSeed, Inputs: rubyPHPInputs},
	}
	for i := range rules {
		rules[i].Builtin = true
		rules[i].Origin = "builtin"
	}
	return rules
}

// ResolveRules merges conservative built-ins with the primary checkout's
// committed .cambium.toml rules. Later exact-path rules override built-ins.
func ResolveRules(repoRoot string) ([]config.LayerRule, error) {
	custom, err := config.LoadPolicyFile(repoRoot)
	if err != nil {
		return nil, err
	}
	return config.MergeRules(Builtins(), custom)
}

// ResolveRulesAt resolves .cambium.toml from the exact target Git tree. This
// keeps path policy branch-correct in the same way environment receipts are
// branch-correct. Path rules have no local override: what a commit builds with
// is decided by that commit.
func ResolveRulesAt(ctx context.Context, repository gitx.Repository, commit string) ([]config.LayerRule, error) {
	content, present, err := repository.FileAt(ctx, commit, config.PolicyFilename)
	if err != nil {
		return nil, err
	}
	var custom []config.LayerRule
	if present {
		custom, err = config.ParsePolicy(commit+":"+config.PolicyFilename, strings.NewReader(content))
		if err != nil {
			return nil, err
		}
	}
	return config.MergeRules(Builtins(), custom)
}

// Detect returns active existing ignored rules for init/explain output. It uses
// Git's own ignore engine and never infers unknown ignored files.
func Detect(ctx context.Context, repository gitx.Repository) ([]config.LayerRule, error) {
	rules, err := ResolveRules(repository.Root)
	if err != nil {
		return nil, err
	}
	var active []config.LayerRule
	for _, rule := range rules {
		path := filepath.Join(repository.Root, filepath.FromSlash(rule.Path))
		_, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		tracked, err := repository.IsTracked(ctx, rule.Path)
		if err != nil {
			return nil, err
		}
		if tracked {
			continue
		}
		ignored, err := repository.IsIgnored(ctx, rule.Path)
		if err != nil {
			return nil, err
		}
		if ignored || rule.AllowUnignored {
			active = append(active, rule)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Path < active[j].Path })
	return active, nil
}

func mergeStrings(groups ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, group := range groups {
		for _, value := range group {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
