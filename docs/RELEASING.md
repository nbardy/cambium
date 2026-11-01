# Manual releases

Cambium uses local validation and manual GitHub releases. Pushing a version
tag does not build or publish binary assets automatically.

## Validate the source

Run the full [local validation gate](VALIDATION.md) on the intended release
commit. Review any skipped tests and record the source commit, toolchain, and
host details. APFS performance claims also require the actual Mac measurements
described there.

Create an annotated release tag at that validated commit, or check out an
existing release tag. For a new release, replace `vX.Y.Z` with its version:

```bash
git tag -a vX.Y.Z -m 'Release vX.Y.Z'
```

## Build the release archives

Run the following in Bash from the repository root with Go 1.23+, Python 3,
and `tar` installed. Set `release_tag` to the tag above. The source must be
clean and match that tag; the output directory must not already exist.

```bash
set -euo pipefail
release_tag=vX.Y.Z
release_version="${release_tag#v}"
release_dir="$PWD/dist/release-${release_tag}"

test -z "$(git status --porcelain)"
test "$(git rev-parse HEAD)" = "$(git rev-parse "${release_tag}^{commit}")"
mkdir -p "$PWD/dist"
mkdir "$release_dir"

for release_os in linux darwin; do
  for release_arch in amd64 arm64; do
    release_name="cambium-${release_version}-${release_os}-${release_arch}"
    mkdir "$release_dir/$release_name"
    for release_command in cambium git-cambium; do
      CGO_ENABLED=0 GOOS="$release_os" GOARCH="$release_arch" \
        go build -trimpath \
        -ldflags="-s -w -X github.com/nbardy/cambium/internal/cli.Version=${release_version}" \
        -o "$release_dir/$release_name/$release_command" "./cmd/$release_command"
    done
    cp README.md COMPARISONS.md LICENSE NOTICE "$release_dir/$release_name/"
    tar -C "$release_dir" -czf "$release_dir/$release_name.tar.gz" "$release_name"
  done
done

python3 - "$release_dir" <<'PY'
import hashlib
import sys
from pathlib import Path

release_dir = Path(sys.argv[1])
with (release_dir / "SHA256SUMS").open("w", encoding="utf-8") as checksums:
    for archive in sorted(release_dir.glob("*.tar.gz")):
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        checksums.write(f"{digest}  {archive.name}\n")
PY
```

This produces four archives for Linux/macOS on amd64/arm64. Each contains both
CLIs plus the README, comparisons, license, and notice files. The binaries
embed the release version. Cross-compilation verifies the build; platform
runtime and filesystem claims still need tests on the relevant host.

## Publish and verify

Push the selected tag with `git push origin "$release_tag"`. Create a release
for that tag on the [repository's Releases page](https://github.com/nbardy/cambium/releases).
Upload the four `.tar.gz` archives and `SHA256SUMS` from `release_dir`, add the
release notes and validation scope, and publish the release.

Download all five assets into an empty directory and verify them with
`sha256sum -c SHA256SUMS` on Linux or `shasum -a 256 -c SHA256SUMS` on macOS.
Extract the archive for the host, enter its directory, and run
`./cambium --version` and `./git-cambium --version`. Both must report the
release version before announcing it.

`scripts/publish-github.sh` publishes source and tags only; it does not build
or upload these release assets.
