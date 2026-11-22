#!/bin/sh
# Builds the libretro cores the component runs, INSIDE a throwaway Debian trixie container (the same Debian as the runtime image, so the
# .so files link against the glibc/libstdc++/libGL they will really run with), and writes them to ./cores (or $CORES_DIR). Nothing is
# built into the component image: you mount that directory read-only at /opt/cores.
#
#   scripts/fetch-cores.sh                        mupen64plus_next + ppsspp (+ the PPSSPP assets, through scripts/psp-assets.sh)
#   scripts/fetch-cores.sh ppsspp
#   scripts/fetch-cores.sh --i-accept-snes9x-noncommercial snes9x     OPT-IN, see the licence note below
#
# Licences (check them yourself before distributing anything you build; this is not legal advice):
#   mupen64plus_next  GPL-2.0 (GLideN64 and mupen64plus parts GPL-2.0-or-later)      compatible with this GPL-3.0-or-later component as a separate program
#   ppsspp            GPL-2.0-or-later (hrydgard/ppsspp, libretro/ dir)               idem; needs NO PSP firmware/BIOS (high level emulation)
#   snes9x            NON-COMMERCIAL licence (not free software, not GPL): it must never be put in an image or distributed; building it for your own
#                     private use is your decision, hence the explicit flag. The default build does not touch it.
# No ROM, firmware or BIOS is downloaded or needed. Pinned COMMIT SHAs (override with env, 40 hex digits only: a tag or branch moves): MUPEN_REF, PPSSPP_REF
# (v1.20.4 = fa50bb1976065c4f8b1b47af227d367fe9771555), SNES9X_REF. After every checkout `git rev-parse HEAD` must equal the pin or the build stops.
# Needs: docker (or DOCKER=podman), network, ~6 GB of disk for ppsspp (clone with submodules + build), a few GB less for mupen. Several minutes.
set -eu
MUPEN_REF=${MUPEN_REF:-15bada85906593376e8fd697f1b0b3af3a20f512}
PPSSPP_REF=${PPSSPP_REF:-fa50bb1976065c4f8b1b47af227d367fe9771555} # tag v1.20.4 (the tag object is 3a31057b...; this is the commit it points to)
SNES9X_REF=${SNES9X_REF:-204ea1c0b851f960cbf223075eb1c25641cb483c}
JOBS=${JOBS:-4}
for r in "$MUPEN_REF" "$PPSSPP_REF" "$SNES9X_REF"; do
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
    mupen64plus_next|ppsspp) want="$want $a" ;;
    snes9x) want="$want $a" ;;
    *) echo "unknown argument: $a (cores: mupen64plus_next ppsspp snes9x)" >&2; exit 2 ;;
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
$D run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e MUPEN_REF="$MUPEN_REF" -e PPSSPP_REF="$PPSSPP_REF" -e SNES9X_REF="$SNES9X_REF" -e JOBS="$JOBS" \
  -v "$out:/out" -v "$here:/scripts:ro" -v /work -w /work "$img" sh /scripts/fetch-cores.sh --inner $want
echo "cores in $out:"; ls -l "$out"
echo "remove the helper image with: $D rmi $img"
