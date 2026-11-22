#!/bin/bash
# The local SSH entry in the installed layout: builds the workbench test binary
# as cage and cage-attach beside it, with the Makefile's flags and one stamp,
# and runs TestEntryInstalledLayout there with a temporary HOME (entry on, ssh
# through the slim cage-attach proxy, a pty session, and with a scratch
# herdr-core a Herdr link). Nothing reads or writes the real profile or ~/.ssh.
# Usage: scripts/entry-installed-layout.sh <repository> <empty scratch folder> [herdr-core]
set -euo pipefail
WT="$1"; OUT="$2"; CORE="${3:-}"
L="$OUT/layout"; HOMEDIR="$OUT/home"
rm -rf "$L" "$HOMEDIR"; mkdir -p "$L" "$HOMEDIR"
HOMEDIR="$(cd "$HOMEDIR" && pwd -P)"
cd "$WT"
STAMP="$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
frag="$OUT/flags.mk"
printf '.PHONY: f\nf:\n\t@echo "TAGS=$(TAGS)"\n\t@echo "LDFLAGS=$(LDFLAGS)"\n' > "$frag"
flags="$(make -s -f Makefile -f "$frag" STAMP="$STAMP" f)"
TAGS="$(printf '%s\n' "$flags" | sed -n 's/^TAGS=//p')"
LDFLAGS="$(printf '%s\n' "$flags" | sed -n 's/^LDFLAGS=//p')"
echo "stamp $STAMP"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$L/cage-attach" ./cmd/cage-attach
go test -c -trimpath -tags "$TAGS" -ldflags "$LDFLAGS" -o "$L/cage" ./internal/workbench
ls -l "$L"
cd "$WT/internal/workbench"
env HOME="$HOMEDIR" CAGE_ENTRY_INSTALLED=1 CAGE_HERDR_CORE="$CORE" "$L/cage" -test.run '^TestEntryInstalledLayout$' -test.v -test.count=1 -test.timeout 5m
