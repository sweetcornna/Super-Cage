#!/usr/bin/env bash
# Package the no-virtualization edition: the gateway and portal, the Linux
# Super Cage server that goes into the workspace template, and the root installer
# that sets up Incus system containers. No container image is bundled; the
# installer builds the blank template on the host.
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$repo_dir"
version=$(<VERSION)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  printf 'Invalid VERSION\n' >&2
  exit 1
}
clean() { [[ -z $(git status --porcelain --untracked-files=normal) ]]; }
clean || {
  printf 'Build release packages from a clean Git checkout.\n' >&2
  exit 1
}
revision=$(git rev-parse HEAD)
if command -v sha256sum >/dev/null; then
  checksum=(sha256sum)
else
  checksum=(shasum -a 256)
fi
stamped() {
  local info
  info=$(go version -m "$1")
  [[ "$info" == *$'\tbuild\tvcs.revision='"$revision"$'\n'* && "$info" == *$'\tbuild\tvcs.modified=false'* ]]
}
mkdir -p _output
output=$(mktemp -d "$repo_dir/_output/tenant-novirt.XXXXXX")
tar_options=()
if tar --version 2>/dev/null | grep -q bsdtar; then
  tar_options=(--no-mac-metadata --no-xattrs)
fi
archives=()
for arch in amd64 arm64; do
  name="Super-Cage-NoVirt-$version-linux-$arch"
  tree="$output/$name"
  mkdir -p "$tree/ui/tenant" "$tree/docs" "$tree/deploy/tenant"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=true -o "$tree/cage-tenant" ./cmd/cage-tenant
  make cage GOOS=linux GOARCH="$arch" CGO_ENABLED=0 > "$output/make-$arch.log" 2>&1
  cp _output/bin/cage "$tree/cage"
  if [[ "$(uname -s)" == Linux ]] && [[ ("$arch" == amd64 && "$(uname -m)" == x86_64) || ("$arch" == arm64 && "$(uname -m)" == aarch64) ]]; then
    "$tree/cage" verify-vm-assets
    "$tree/cage" verify-web-assets
  fi
  for binary in "$tree/cage-tenant" "$tree/cage"; do
    stamped "$binary" || {
      printf '%s build metadata does not match clean revision %s\n' "$binary" "$revision" >&2
      exit 1
    }
  done
  clean || {
    printf 'The build changed the checkout.\n' >&2
    exit 1
  }
  cp deploy/tenant/novirt/install.sh deploy/tenant/novirt/cage-serve.service "$tree/"
  chmod 0755 "$tree/install.sh" "$tree/cage" "$tree/cage-tenant"
  cp deploy/tenant/novirt/README.md "$tree/README.md"
  cp LICENSE NOTICE "$tree/"
  cp deploy/tenant/README.md "$tree/deploy/tenant/"
  cp docs/api-contract.md "$tree/docs/"
  cp ui/tenant/index.html ui/tenant/app.mjs ui/tenant/styles.css "$tree/ui/tenant/"
  archive="$name.tar.gz"
  COPYFILE_DISABLE=1 tar "${tar_options[@]}" -czf "$output/$archive" -C "$output" "$name"
  gzip -t "$output/$archive"
  archives+=("$archive")
done
(
  cd "$output"
  "${checksum[@]}" "${archives[@]}" > SHA256SUMS
  "${checksum[@]}" -c SHA256SUMS
)
printf 'No-virtualization packages: %s\n' "$output"
