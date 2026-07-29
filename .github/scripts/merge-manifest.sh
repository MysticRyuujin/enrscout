#!/usr/bin/env bash
# usage: merge-manifest.sh <image> <digest-dir> <tag-ref> [tag-ref...]  -> prints index digest
#
# Release and CI both call this, so the release path has no second copy to drift from.
# <digest-dir> holds one empty file per architecture, named for its digest sans "sha256:".
set -euo pipefail

if [[ "$#" -lt 3 ]]; then
  echo "usage: $(basename "$0") <image> <digest-dir> <tag-ref> [tag-ref...]" >&2
  exit 2
fi

image="$1"
digest_dir="$2"
shift 2
tags=("$@")

digests=()
for path in "$digest_dir"/*; do
  [[ -f "$path" ]] || continue
  digests+=("$(basename "$path")")
done
if [[ "${#digests[@]}" -ne 2 ]]; then
  echo "::error::Expected two platform digests, found ${#digests[@]}"
  exit 1
fi

refs=()
for digest in "${digests[@]}"; do
  refs+=("${image}@sha256:${digest}")
done

# Check the assembled index before any tag moves: create writes every --tag it is given, so
# validating afterwards would leave a bad index published under the release tags.
# Attestation manifests ride along as unknown/unknown children; only real platforms count.
index="$(docker buildx imagetools create --dry-run "${refs[@]}")"
platforms="$(
  jq -r '.manifests[].platform | select(.os != "unknown") | "\(.os)/\(.architecture)"' \
    <<< "$index" | sort -u | paste -sd, -
)"
if [[ "$platforms" != "linux/amd64,linux/arm64" ]]; then
  echo "::error::Unexpected image platforms: ${platforms:-none}"
  exit 1
fi

tag_args=()
for tag in "${tags[@]}"; do
  tag_args+=(--tag "$tag")
done
docker buildx imagetools create "${tag_args[@]}" "${refs[@]}"

digest="$(
  docker buildx imagetools inspect "${tags[0]}" |
    awk '$1 == "Digest:" {print $2; exit}'
)"
if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "::error::Could not resolve the multi-platform image digest"
  exit 1
fi

echo "$digest"
