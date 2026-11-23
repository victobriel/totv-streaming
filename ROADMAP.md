# Roadmap and status

## Status

| Platform | State |
|---|---|
| SNES (snes9x, software render) | Proved on the author's machine |
| N64 (mupen64plus-next + GLideN64, GPU via headless EGL) | Proved on the author's machine |
| PSP (PPSSPP, OpenGL) | Proved on the author's machine (homebrew and one commercial game up to title/intro) |
| PS2 (PCSX2 libretro core `parallel-ps2`/LRPS2, OpenGL) | **Prototype, no game ever run**: the core builds, loads, creates the GL context, runs its JIT and threads under the full sandbox, and the BIOS/ISO/cards/input/zip plumbing is proved with a synthetic BIOS and a test core. Needs the owner's own BIOS and image to go further (README "PS2") |

"Proved" means one Linux PC with one NVIDIA GPU, a pion test client and a fake Totv; see README "What is NOT proved".

## Pending

- **PS2 (needs the owner's files):** run `PS2=1 PS2_ROM=<image> PS2_BIOS_DIR=<dir> make ps2` with a real BIOS dump and a game image of the owner's own console: frame rate, CPU/GPU/RAM of real gameplay, latency, memory-card saves written by a game, long runs, the GL renderer's per-game compatibility. Evolutions noted, not done: reading the image without a full copy (Range/fd sharing), a shared read-only shader cache across sessions, Vulkan/paraLLEl-GS (the host only creates the OpenGL context).

- **Licenses:** confirm the exact licenses of mupen64plus-next and GLideN64 at the pinned commits; legal advice on the
  snes9x non-commercial restriction and on distributing builds.
- **Relicense `emu/` to GPL-2.0-or-later (owner decision):** `emu/host.c` loads the core in-process; GPL-3.0-or-later host plus a
  GPL-2.0-only core cannot be distributed together. `emu/` is own code (libretro.h stays MIT), so relicensing is possible; evaluate
  the interaction with x264 and the Go side before changing anything. Not applied.
- **Publication notes:** the repository history is short (squashed first publication plus follow-up fixes); the CI "full suite" step is informational (`continue-on-error`), only the
  pure tests gate. If an image is published, provide the source offer for the Debian packages (libx264 included).
- **Docker + GPU:** run the image with a GPU (NVIDIA Container Toolkit) and prove sessions in it.
- **microVM** with its own GPU for untrusted ROMs (the deny-list sandbox is not enough for multi-user use).
- **Publish the image** (`ghcr.io/victobriel/totv-streaming` in the compose example is only an example name today).
- Real Android/Fire TV WebRTC client, latency measurement, 30-minute stability, congestion control.
- Integration tests against the real Totv instead of the fake.
