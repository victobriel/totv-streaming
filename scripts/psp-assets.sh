#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 victobriel
# Installs the PPSSPP system assets next to the cores: scripts/psp-assets.sh <ppsspp source tree> <system dir>
# (<system dir> is TOTV_STREAM_PSP_SYSTEM, default <cores dir>/system; the core reads <system dir>/PPSSPP/..., mounted read-only in its sandbox).
# Minimal set (1.9 MB, enough to boot and run a homebrew, an ISO and a CSO headless, measured): ppge_atlas.zim + ppge_atlas.meta (the PSP's
# on-screen dialogs: the .meta holds the glyph table, WITHOUT it every PPGe dialog (savedata "There is no data.") is a blank box with no text,
# which looks like a freeze), font_atlas.zim + font_atlas.meta, compat.ini (per-game compatibility flags: needed for commercial games), langregion.ini, lang/en_US.ini.
# NOT installed: assets/flash0/font/*.pgf (PPSSPP's PSP font files; without them the core logs "using the HLE sceFont" and runs),
# shaders, themes, the debugger UI, sound effects. FULL=1 installs the whole assets/ directory (17 MB, without the debugger) instead.
# No PSP firmware/BIOS is needed or used: PPSSPP is a high level emulation of it.
set -eu
src=${1:?usage: psp-assets.sh <ppsspp source tree> <system dir>}; dest=${2:?}
[ -f "$src/assets/ppge_atlas.zim" ] || { echo "not a PPSSPP tree: $src" >&2; exit 1; }
[ -f "$src/assets/ppge_atlas.meta" ] || { echo "$src/assets/ppge_atlas.meta missing: PSP dialogs would have no text" >&2; exit 1; }
rm -rf "$dest/PPSSPP"; mkdir -p "$dest/PPSSPP/lang"
if [ "${FULL:-0}" = 1 ]; then
  cp -r "$src/assets/." "$dest/PPSSPP/"; rm -rf "$dest/PPSSPP/debugger"
else
  for f in ppge_atlas.zim ppge_atlas.meta font_atlas.zim font_atlas.meta compat.ini langregion.ini; do cp "$src/assets/$f" "$dest/PPSSPP/$f"; done
  cp "$src/assets/lang/en_US.ini" "$dest/PPSSPP/lang/en_US.ini"
fi
echo "installed $(du -sh "$dest/PPSSPP" | cut -f1) in $dest/PPSSPP"
