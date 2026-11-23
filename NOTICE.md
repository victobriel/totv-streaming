# Third-party notices

This project is GPL-3.0-or-later (see [LICENSE](LICENSE)). This is an informal summary, **not legal advice**; check each
upstream license before distributing anything you build. Nothing below is bundled in this repository except where
marked "in repo".

## libretro cores (built by you with `scripts/fetch-cores.sh`, not distributed here)

| Core | License | Notes |
|---|---|---|
| snes9x | **Non-commercial** (not free software, not GPL) | Opt-in only (`--i-accept-snes9x-noncommercial`). Never put in an image or distribute binaries. https://github.com/snes9xgit/snes9x/blob/master/LICENSE |
| mupen64plus-libretro-nx (mupen64plus-next) | GPL-2.0, some parts "or later"; **to be confirmed** at the pinned commit (check the upstream tree) | https://github.com/libretro/mupen64plus-libretro-nx |
| GLideN64 (inside mupen64plus-next) | GPL-2.0, parts "or later"; **to be confirmed** at the pinned commit | https://github.com/gonetz/GLideN64 |
| PPSSPP (libretro build) | GPL-2.0-or-later | https://github.com/hrydgard/ppsspp/blob/master/LICENSE.TXT |

**GPL-2.0 versus GPL-3.0 (read this).** `emu/host.c` (GPL-3.0-or-later) loads each core with `dlopen` into the **same process**; it is
not a separate program. A GPL-3.0-or-later host together with a GPL-2.0-**only** core is incompatible for **joint distribution**
(for example an image or archive holding both). Building and running them on your own machine distributes nothing, so the
local use this project documents is not affected. The cores are never distributed here. Proposal, **not applied, the owner
decides**: relicense `emu/` (own code) as GPL-2.0-or-later, which is compatible with GPL-2.0-only cores and with GPL-3.0 parts
(listed in [ROADMAP.md](ROADMAP.md)). Note that the Go program links x264 (GPL-2.0-or-later) and talks to the host over pipes,
which is a separate question from the in-process core loading.

PPSSPP assets (`ppge_atlas.zim`, `font_atlas.zim`, `compat.ini`, language files) are installed by `scripts/psp-assets.sh`
from the PPSSPP source tree and follow the PPSSPP license. In repo: its license text
(`scripts/psp-assets-PPSSPP-LICENSE.txt`). No PSP firmware or BIOS is used.

## Linked or compiled-in libraries

| Component | License |
|---|---|
| x264 (cgo) | GPL-2.0-or-later, https://www.videolan.org/developers/x264.html (this is why the combined work is GPL) |
| libopus (cgo) | BSD-3-Clause, https://opus-codec.org/license/ |
| pion/webrtc and other pion modules | MIT, https://github.com/pion/webrtc/blob/master/LICENSE |
| Other Go modules | see `go.mod`/`go.sum`; `go-licenses` can list them |
| libretro.h (in repo: `emu/libretro.h`) | MIT, header carries its notice; RetroArch team |
| PSPSDK cube example (in repo: `scripts/psp-homebrew`) | BSD-style, `scripts/psp-homebrew/LICENSE-PSPSDK`; https://github.com/pspdev/pspsdk |

## Docker image and derived files

| Item | License / note |
|---|---|
| `docker/seccomp-totv.json` | Derived from the default seccomp profile of Docker (moby/profiles, https://github.com/moby/profiles), **Apache-2.0**, plus three extra rules added by `scripts/make-seccomp.sh`. Copyright the Moby/Docker authors for the base part. |
| Docker image (`Dockerfile`) | Built on Debian and installs Debian packages, among them **libx264** (GPL-2.0-or-later) and its dependencies. **Open question:** if an image is ever published, the written offer / source for the Debian packages (and the complete corresponding source of this project) must accompany it. No image is published today. |

## Your own content

ROMs, BIOS and firmware are not distributed with this project and must be legally obtained by you.
