# Security policy

Cambium is not a security sandbox. It isolates workspace files and Git state,
not processes, users, networks, credentials, ports, or files outside the
workspace.

Please report vulnerabilities involving unintended deletion, branch loss,
path traversal, tracked-path overlay, secret inference, symlink escape, unsafe
recovery, or command injection privately through GitHub security advisories
once the repository is published.

See [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md).
