#!/bin/bash
# read-only size survey
T="${TMPDIR%/}"
sz() { printf '%s\t%s\n' "$(du -sh "$1" 2>/dev/null | cut -f1)" "$1"; }
echo "== Library/Caches top"
du -sh "$HOME"/Library/Caches/* 2>/dev/null | sort -rh | head -20
echo "== Xcode"
for p in DerivedData Archives "iOS DeviceSupport" "watchOS DeviceSupport"; do sz "$HOME/Library/Developer/Xcode/$p"; done
sz "$HOME/Library/Developer/CoreSimulator"
echo "== TMPDIR ($T)"
sz "$T"
du -sh "$T"/* 2>/dev/null | sort -rh | head -15
echo "tmp.* count: $(ls -d "$T"/tmp.* 2>/dev/null | wc -l)  size:"; du -csh "$T"/tmp.* 2>/dev/null | tail -1
echo "tmp.* older than 1d:"; find "$T" -maxdepth 1 -name 'tmp.*' -mtime +1 -print0 2>/dev/null | xargs -0 du -csh 2>/dev/null | tail -1
echo "paperclip-run-*:"; du -csh "$T"/paperclip-run-* 2>/dev/null | tail -1
echo "== caches"
for p in .npm .npm/_cacache .cache .cache/pip Library/Caches/pip Library/Caches/pnpm Library/pnpm .bun/install/cache .yarn Library/Caches/Yarn go/pkg/mod Library/Caches/go-build .cargo/registry .gradle .m2 .rustup .docker Library/Containers/com.docker.docker .paperclip Documents/dev/agent-mesh/.worktrees Documents/dev/worktrees .Trash Downloads; do sz "$HOME/$p"; done
echo "== .paperclip top"
du -sh "$HOME"/.paperclip/* 2>/dev/null | sort -rh | head -10
echo "== .cache top"
du -sh "$HOME"/.cache/* 2>/dev/null | sort -rh | head -10
