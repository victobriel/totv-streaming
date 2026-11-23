#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Copyright (C) 2026 victobriel
# Builds the libretro cores the component runs, INSIDE a throwaway Debian trixie container (the same Debian as the runtime image, so the
# .so files link against the glibc/libstdc++/libGL they will really run with), and writes them to ./cores (or $CORES_DIR). Nothing is
# built into the component image: you mount that directory read-only at /opt/cores.
#
#   scripts/fetch-cores.sh                        mupen64plus_next + ppsspp (+ the PPSSPP assets, through scripts/psp-assets.sh)
#   scripts/fetch-cores.sh ppsspp
#   scripts/fetch-cores.sh pcsx2                  the PS2 core (LRPS2 / "parallel-ps2"); NOT in the default set: it needs a console BIOS YOU provide (README, "PS2")
#   scripts/fetch-cores.sh --i-accept-snes9x-noncommercial snes9x     OPT-IN, see the licence note below
#
# Licences (check them yourself before distributing anything you build; this is not legal advice):
#   mupen64plus_next  GPL-2.0 (GLideN64 and mupen64plus parts GPL-2.0-or-later): TO BE CONFIRMED at the pinned commit. The core is dlopen()ed INTO THE
#                     SAME PROCESS as emu/host.c (GPL-3.0-or-later), not run as a separate program: a GPL-2.0-only core and this GPL-3.0-or-later
#                     host cannot be DISTRIBUTED together (building and running them locally distributes nothing). See NOTICE.md.
#   ppsspp            GPL-2.0-or-later (hrydgard/ppsspp, libretro/ dir): same dlopen note, but "or later" allows GPL-3.0; needs NO PSP firmware/BIOS
#   pcsx2             GPL-3.0 (libretro/parallel-ps2, README titled LRPS2, derived from PCSX2, GPL-3.0-or-later upstream): compatible with the GPL-3.0-or-later
#                     host. Needs a PS2 BIOS dump of the operator's own console, never downloaded or shipped here. See NOTICE.md.
#   snes9x            NON-COMMERCIAL licence (not free software, not GPL): it must never be put in an image or distributed; building it for your own
#                     private use is your decision, hence the explicit flag. The default build does not touch it.
# No ROM, firmware or BIOS is downloaded by this script. Pinned COMMIT SHAs (override with env, 40 hex digits only: a tag or branch moves): MUPEN_REF, PPSSPP_REF, PCSX2_REF
# (v1.20.4 = fa50bb1976065c4f8b1b47af227d367fe9771555), SNES9X_REF. After every checkout `git rev-parse HEAD` must equal the pin or the build stops.
# Needs: docker (or DOCKER=podman), network, ~6 GB of disk for ppsspp (clone with submodules + build), a few GB less for mupen. Several minutes.
set -eu
MUPEN_REF=${MUPEN_REF:-15bada85906593376e8fd697f1b0b3af3a20f512}
PPSSPP_REF=${PPSSPP_REF:-fa50bb1976065c4f8b1b47af227d367fe9771555} # tag v1.20.4 (the tag object is 3a31057b...; this is the commit it points to)
SNES9X_REF=${SNES9X_REF:-204ea1c0b851f960cbf223075eb1c25641cb483c}
PCSX2_REF=${PCSX2_REF:-77e2e0d86cac1b3dd54465d2547b0e7246b9ec3b} # libretro/parallel-ps2 master of 2026-11-22 (the SHA this was built and tested at)
JOBS=${JOBS:-4}
for r in "$MUPEN_REF" "$PPSSPP_REF" "$SNES9X_REF" "$PCSX2_REF"; do
  case $r in *[!0-9a-f]*|"") echo "refs must be full commit SHAs (40 hex digits), got: $r" >&2; exit 2 ;; esac
  [ ${#r} -eq 40 ] || { echo "refs must be full commit SHAs (40 hex digits), got: $r" >&2; exit 2; }
done
pinned() { # pinned <dir> <sha>: the checked-out commit must be exactly the pin
  [ "$(git -C "$1" rev-parse HEAD)" = "$2" ] || { echo "$1: HEAD is $(git -C "$1" rev-parse HEAD), expected $2: refusing to build" >&2; exit 1; }
}

if [ "${1:-}" = --inner ]; then # ---- runs inside the container (/out is the cores dir, /scripts this directory, /work scratch)
  shift; cd /work
  for c in "$@"; do
    case $c in
      mupen64plus_next)
        git clone --depth 1 https://github.com/libretro/mupen64plus-libretro-nx m64 && git -C m64 fetch --depth 1 origin "$MUPEN_REF" && git -C m64 checkout -q FETCH_HEAD && pinned m64 "$MUPEN_REF"
        make -C m64 -j"$JOBS" && strip -s m64/mupen64plus_next_libretro.so && cp m64/mupen64plus_next_libretro.so /out/ ;;
      ppsspp)
        git clone https://github.com/hrydgard/ppsspp ppsspp && git -C ppsspp checkout -q "$PPSSPP_REF" && pinned ppsspp "$PPSSPP_REF" && git -C ppsspp submodule update --init --recursive --depth 1 -j4
        make -C ppsspp/libretro -j"$JOBS" && strip -s ppsspp/libretro/ppsspp_libretro.so && cp ppsspp/libretro/ppsspp_libretro.so /out/
        sh /scripts/psp-assets.sh ppsspp /out/system ;;
      pcsx2)
        # IS_X86=1: the Makefile derives it from `uname -p`, which prints "unknown" on many distributions (then no -msse4.1 and the GS fails to build).
        # HAVE_VULKAN=0: the host serves the OpenGL 4.6 context only; the Vulkan/paraLLEl-GS renderers are not built. No external library is needed.
        git clone --depth 1 https://github.com/libretro/parallel-ps2 pcsx2 && git -C pcsx2 fetch --depth 1 origin "$PCSX2_REF" && git -C pcsx2 checkout -q FETCH_HEAD && pinned pcsx2 "$PCSX2_REF"
        make -C pcsx2 -j"$JOBS" IS_X86=1 HAVE_VULKAN=0 HAVE_OPENGL=1 && strip -s pcsx2/pcsx2_libretro.so && cp pcsx2/pcsx2_libretro.so /out/ ;;
      snes9x)
        git clone https://github.com/libretro/snes9x snes9x && git -C snes9x checkout -q "$SNES9X_REF" && pinned snes9x "$SNES9X_REF"
        make -C snes9x/libretro -j"$JOBS" && strip -s snes9x/libretro/snes9x_libretro.so && cp snes9x/libretro/snes9x_libretro.so /out/ ;;
    esac
  done
  exit 0
fi

want=""; ok_snes=0
for a in "$@"; do
  case $a in
    --i-accept-snes9x-noncommercial) ok_snes=1 ;;
    mupen64plus_next|ppsspp|pcsx2) want="$want $a" ;;
    snes9x) want="$want $a" ;;
    *) echo "unknown argument: $a (cores: mupen64plus_next ppsspp pcsx2 snes9x)" >&2; exit 2 ;;
  esac
done
[ -n "$want" ] || want=" mupen64plus_next ppsspp"
case $want in *snes9x*) [ "$ok_snes" = 1 ] || { cat >&2 <<'MSG'
snes9x is under a NON-COMMERCIAL licence: it is not free software and must not be shipped in an image or redistributed.
Building it for strictly private use is up to you. If that is what you want, repeat the command with:
    scripts/fetch-cores.sh --i-accept-snes9x-noncommercial snes9x
MSG
  exit 3; } ;; esac

here=$(cd "$(dirname "$0")" && pwd); root=$(dirname "$here")
out=${CORES_DIR:-$root/cores}; mkdir -p "$out"; out=$(cd "$out" && pwd)
D=${DOCKER:-docker}
img=totv-streaming-corebuild:local
# shellcheck disable=SC2086  (BUILD_FLAGS is word-split on purpose, e.g. "--load" with a docker-container buildx builder)
$D build ${BUILD_FLAGS:-} --target corebuild -t "$img" "$root"
# default network (needs github); nothing from the host is mounted besides the output dir and these scripts (read-only); /work is an anonymous
# volume (disk, removed with the container: the build tree is several GB and must not live in RAM)
$D run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e MUPEN_REF="$MUPEN_REF" -e PPSSPP_REF="$PPSSPP_REF" -e SNES9X_REF="$SNES9X_REF" -e PCSX2_REF="$PCSX2_REF" -e JOBS="$JOBS" \
  -v "$out:/out" -v "$here:/scripts:ro" -v /work -w /work "$img" sh /scripts/fetch-cores.sh --inner $want
echo "cores in $out:"; ls -l "$out"
echo "remove the helper image with: $D rmi $img"
