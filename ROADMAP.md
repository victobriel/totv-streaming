# Roadmap and status

## Status

| Platform | State |
|---|---|
| SNES (snes9x, software render) | Proved on the author's machine |
| N64 (mupen64plus-next + GLideN64, GPU via headless EGL) | Proved on the author's machine |
| PSP (PPSSPP, OpenGL) | Proved on the author's machine (homebrew and one commercial game up to title/intro) |
| PS2 | Not done |

"Proved" means one Linux PC with one NVIDIA GPU, a pion test client and a fake Totv; see README "What is NOT proved".

## Pending

- **Licenses:** confirm the exact licenses of mupen64plus-next and GLideN64 at the pinned commits; legal advice on the
  snes9x non-commercial restriction and on distributing builds.
- **Docker + GPU:** run the image with a GPU (NVIDIA Container Toolkit) and prove sessions in it.
- **microVM** with its own GPU for untrusted ROMs (the deny-list sandbox is not enough for multi-user use).
- **Publish the image** (`ghcr.io/victobriel/totv-streaming` in the compose example is only an example name today).
- Real Android/Fire TV WebRTC client, latency measurement, 30-minute stability, congestion control.
- Integration tests against the real Totv instead of the fake.
