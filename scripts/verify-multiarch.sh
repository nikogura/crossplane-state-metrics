#!/usr/bin/env bash
#
# Verify that every architecture in a multi-arch OCI layout actually carries a
# binary of that architecture.
#
# Declaring two platforms to buildx is not the same as shipping two correct
# binaries. A per-arch manifest holding the wrong ELF passes a plain build, an
# image push, and every manifest-list inspection - and then fails at runtime on
# the machines least likely to be the ones you tested on.
set -euo pipefail

layout="${1:?usage: verify-multiarch.sh <oci-layout-dir> <binary-name>}"
binary="${2:?usage: verify-multiarch.sh <oci-layout-dir> <binary-name>}"

index="$(jq -r '.manifests[0].digest' "$layout/index.json" | sed 's/sha256://')"

mapfile -t entries < <(
  jq -r '.manifests[] | select(.platform.architecture != "unknown")
         | "\(.platform.architecture) \(.digest)"' "$layout/blobs/sha256/$index"
)

if [ "${#entries[@]}" -lt 2 ]; then
  echo "expected at least two architectures, found ${#entries[@]}" >&2
  exit 1
fi

status=0

for entry in "${entries[@]}"; do
  arch="${entry%% *}"
  digest="${entry##* }"
  digest="${digest#sha256:}"

  layer="$(jq -r '.layers[-1].digest' "$layout/blobs/sha256/$digest" | sed 's/sha256://')"

  workdir="$(mktemp -d)"
  tar -xzf "$layout/blobs/sha256/$layer" -C "$workdir" 2>/dev/null || true

  path="$(find "$workdir" -name "$binary" -type f | head -1)"
  if [ -z "$path" ]; then
    echo "FAIL ${arch}: binary ${binary} not found in the final layer" >&2
    status=1
    rm -rf "$workdir"
    continue
  fi

  described="$(file -b "$path")"

  case "$arch" in
    amd64) expected="x86-64" ;;
    arm64) expected="aarch64" ;;
    *)     echo "FAIL ${arch}: no expected ELF machine mapping" >&2; status=1; rm -rf "$workdir"; continue ;;
  esac

  if echo "$described" | grep -q "$expected"; then
    echo "ok   ${arch}: ${described%%,*}, ${expected}"
  else
    echo "FAIL ${arch}: manifest claims ${arch} but binary is: ${described}" >&2
    status=1
  fi

  rm -rf "$workdir"
done

exit "$status"
