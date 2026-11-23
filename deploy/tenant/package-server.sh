#!/usr/bin/env bash
# Package the Linux gateway and its static Web portal. No VM images are bundled.
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$repo_dir"
version=$(<VERSION)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  printf 'Invalid VERSION\n' >&2
  exit 1
}
if [[ -n $(git status --porcelain --untracked-files=normal) ]]; then
  printf 'Build release packages from a clean Git checkout.\n' >&2
  exit 1
fi
revision=$(git rev-parse HEAD)
if command -v sha256sum >/dev/null; then
  checksum=(sha256sum)
else
  checksum=(shasum -a 256)
fi
mkdir -p _output
output=$(mktemp -d "$repo_dir/_output/tenant-server.XXXXXX")
# A tarball made on a Mac must not carry AppleDouble ._ files or extended
# attributes: Linux sees them as stray files and tar warnings.
tar_options=()
if tar --version 2>/dev/null | grep -q bsdtar; then
  tar_options=(--no-mac-metadata --no-xattrs)
fi
archives=()
for arch in amd64 arm64; do
  name="Super-Cage-Server-$version-linux-$arch"
  tree="$output/$name"
  mkdir -p "$tree/ui/tenant" "$tree/docs" "$tree/deploy/tenant" "$tree/deploy/cloud"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=true -o "$tree/cage-tenant" ./cmd/cage-tenant
  build_info=$(go version -m "$tree/cage-tenant")
  if [[ "$build_info" != *$'\tbuild\tvcs.revision='"$revision"$'\n'* || "$build_info" != *$'\tbuild\tvcs.modified=false' ]]; then
    printf 'Gateway build metadata does not match clean revision %s\n' "$revision" >&2
    exit 1
  fi
  cp LICENSE NOTICE "$tree/"
  cp deploy/tenant/README.md "$tree/deploy/tenant/"
  cp deploy/cloud/README.md "$tree/deploy/cloud/"
  cp docs/api-contract.md "$tree/docs/"
  cp ui/tenant/index.html ui/tenant/app.mjs ui/tenant/styles.css "$tree/ui/tenant/"
  archive="$name.tar.gz"
  COPYFILE_DISABLE=1 tar ${tar_options[@]+"${tar_options[@]}"} -czf "$output/$archive" -C "$output" "$name"
  gzip -t "$output/$archive"
  archives+=("$archive")
done
(
  cd "$output"
  "${checksum[@]}" "${archives[@]}" > SHA256SUMS
  "${checksum[@]}" -c SHA256SUMS
)
printf 'Server packages: %s\n' "$output"