# Third-party notices

This project is GPL-3.0-or-later (see [LICENSE](LICENSE)). This is an informal summary, **not legal advice**; check each
upstream license before distributing anything you build. Nothing below is bundled in this repository except where
marked "in repo".

## libretro cores (built by you with `scripts/fetch-cores.sh`, not distributed here)

| Core | License | Notes |
|---|---|---|
| snes9x | **Non-commercial** (not free software, not GPL) | Opt-in only (`--i-accept-snes9x-noncommercial`). Never put in an image or distribute binaries. https://github.com/snes9xgit/snes9x/blob/master/LICENSE |
| mupen64plus-libretro-nx (mupen64plus-next) | **To be confirmed** (mix of GPL-2.0 and GPL-3.0 parts; check the upstream tree at the pinned commit) | https://github.com/libretro/mupen64plus-libretro-nx |
| GLideN64 (inside mupen64plus-next) | **To be confirmed** (upstream states GPL-2.0-or-later) | https://github.com/gonetz/GLideN64 |
| PPSSPP (libretro build) | GPL-2.0-or-later | https://github.com/hrydgard/ppsspp/blob/master/LICENSE.TXT |

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

## Your own content

ROMs, BIOS and firmware are not distributed with this project and must be legally obtained by you.
