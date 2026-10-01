#!/usr/bin/env sh
set -eu

CAMBIUM_BIN=${CAMBIUM_BIN:-cambium}
OUTPUT=${OUTPUT:-cambium-benchmark.json}
METHODS=${METHODS:-git,git-env-copy,cambium-auto,cambium-git,cambium-cow,cambium-copy,simgit,cow}
COUNT=${COUNT:-8}
FILES=${FILES:-10000}
FILE_BYTES=${FILE_BYTES:-1024}
ENV_FILES=${ENV_FILES:-2000}
ENV_BYTES=${ENV_BYTES:-2048}

exec "$CAMBIUM_BIN" benchmark \
  --methods "$METHODS" \
  --count "$COUNT" \
  --files "$FILES" \
  --file-bytes "$FILE_BYTES" \
  --env-files "$ENV_FILES" \
  --env-file-bytes "$ENV_BYTES" \
  --output "$OUTPUT"
