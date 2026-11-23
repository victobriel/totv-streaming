> Development log kept from the prototype phase (measurements on the author's machine). The project overview is in the top-level README.md.

# totv-streaming (prototype 1: SNES; prototype 2: N64 on the GPU, see "N64"; prototype 3: PSP, see "PSP"; prototype 4: PS2, see "PS2")

Streaming component for the Totv (contract: `docs/games-streaming.md` §5, §8, §9 and
`docs/games-streaming.md` of the Totv repository). Runs ONE libretro core (SNES, N64, PSP or PS2) per session and sends
H.264 Baseline (no B-frames) + Opus over WebRTC, receiving the controller over an unordered DataChannel.

License: GPL-3.0-or-later (see `LICENSE`). Separate repository from the Totv; nothing here goes into the Totv image.
**snes9x has a non-commercial license: internal prototype, do not distribute binaries or images with it.**
This is a prototype, not a product.

**Status:** SNES (snes9x, software render) and N64 (mupen64plus-next + GLideN64 on the GPU through a headless EGL context) are integrated on
`master` and proved on this PC (see "N64" and the tables below). **PSP (PPSSPP, OpenGL) is a prototype on the branch `psp`** (see "PSP"): it runs
on the same GPU path at 60 fps with homebrew/own-built ISO and CSO images, and with one commercial game (a commercial PSP game, up to its title/intro screens: 59.94 fps, ~35% of a core), **but real gameplay load is not
measured**. **PS2 (PCSX2 libretro core) is a prototype on the branch `ps2`, no game has ever run on it** (no BIOS and no PS2 image on this machine): see "PS2" for exactly what was and was not proved.

## Stack and why

| Piece | Choice | Why |
|---|---|---|
| Control, signaling, WebRTC | Go + [pion/webrtc v4](https://github.com/pion/webrtc) | Pure Go WebRTC with direct access to RTP packets and header extensions (needed for playout-delay 0/0), UDP mux on one fixed port, no C WebRTC build. Least effort for the control surface. |
| H.264 | libx264 through cgo (`superfast` + `zerolatency`, Baseline, CRF 22 under a VBV cap, `bframes=0`, no lookahead, 4 sliced threads; see "Picture quality") | One frame in, one access unit out, in-process: no pipe, no AU framing delay (an `ffmpeg` subprocess would add a hop and a frame of splitting delay). Measured ~1 ms per 640x480 frame at `ultrafast`, ~2 ms (p95 3 ms) per 960x720 frame at the current `superfast`. |
| Opus | libopus through cgo (`RESTRICTED_LOWDELAY`, 20 ms) | Smallest delay Opus offers. Linear resampling from the core rate to 48 kHz (prototype quality). |
| Emulator | `retro-emu`, a small C libretro host (`emu/host.c`), separate process | Single-threaded (Go's multi-threaded runtime is awkward for Landlock, which is per-thread). Order: ROM and save are read into memory, the core is `dlopen`ed (`RTLD_NOW`; its constructors run, no libretro function does), the sandbox is applied, and only then `retro_init`/`retro_load_game` run. Loads any `*_libretro.so`. Converts to I420 and scales (nearest) to 640x480 in C. |
| Test core | `emu/testcore` (own libretro core, no ROM needed) | Gives tests a real core: moving square, 440 Hz tone, screen turns white + tone 880 Hz while RetroPad A is held (latency probe), 8 KiB save RAM whose first counter is incremented on each load (save test). |

Not chosen: Sunshine/Moonlight (rejected in the contract), a Go libretro binding (extra dependency for 300 lines of C).

## Build and test (no commercial ROM anywhere)

Needs: Go >= 1.24, gcc, make, libx264 and libopus dev packages (`pkg-config x264 opus`). ffmpeg only for the latency probe.
Optional, for the GPU path (N64): the EGL and GL headers (`/usr/include/EGL/egl.h`, `/usr/include/GL/glext.h`, e.g. `libegl-dev`); the Makefile
defines `HAVE_EGL` only when they exist (without them `retro-emu` still builds, GPU cores are refused), `libEGL.so.1` is `dlopen()`ed at run time and
a GPU driver with EGL device support is needed to run it. Building the N64 core itself (outside this repo, see "Core choice") also needs `g++` and `nasm`
(x86-64 dynarec). Cores (`cores/*.so`) and `bin/` are ignored by git; ROMs are never part of the repository.

```
make build     # bin/retro-emu, bin/testcore_libretro.so, bin/totv-streaming
make test      # go vet + go test ./... (rebuilds the C parts first)
make latency   # PROBE=1 latency probe, prints percentiles
make gl        # GPU path without any ROM (triangle core) + its latency probe (needs a GPU: skipped otherwise)
make n64       # N64=1: real a commercial N64 game on the GPU (needs cores/mupen64plus_next_libretro.so and a ROM outside the repo)
make psp       # PSP=1 (PSP_KIND=game for a commercial ROM, see "PSP"): real PPSSPP on the GPU (needs cores/ppsspp_libretro.so, cores/system/PPSSPP and PSP_ROM=<file outside the repo>)
```

Run (development, with the test core, any file works as "ROM"):

```
export TOTV_STREAM_SOCK=/tmp/rs/control.sock TOTV_STREAM_SECRET_FILE=/tmp/rs/secret \
       TOTV_STREAM_CORES=$PWD/bin TOTV_STREAM_EMU=$PWD/bin/retro-emu TOTV_STREAM_DEV_TESTCORE=1
mkdir -p /tmp/rs && echo 'a-secret' > /tmp/rs/secret && chmod 600 /tmp/rs/secret
bin/totv-streaming
curl --unix-socket /tmp/rs/control.sock -H 'Authorization: Bearer a-secret' http://x/v1/health
```

Environment variables: `TOTV_STREAM_SOCK` (default `/run/totv-stream/control.sock`), `TOTV_STREAM_SECRET_FILE`
(`/run/totv-stream-secret/retro-stream.secret`), `TOTV_STREAM_CORES` (`/opt/cores`), `TOTV_STREAM_EMU`,
`TOTV_STREAM_UDP_PORT` (8422), `TOTV_STREAM_TMP`, `TOTV_STREAM_WIDTH/HEIGHT/KBPS` (640/480/4000),
`TOTV_STREAM_NOSIGNAL_SEC` (60), `TOTV_STREAM_DISCONNECT_SEC` (30),
`TOTV_STREAM_DEV_TESTCORE=1` (also offers `server_testcore` and `server_glcore`, never in production),
`TOTV_STREAM_GPU_DEVICES` (`/dev/nvidiactl`: device nodes a GPU core may still open after the sandbox, see "N64"),
`TOTV_STREAM_READBACK` (`sync` default, `pbo`), `TOTV_STREAM_N64_RES` (`2x` = 640x480 internal is the default of the `high` profile, `native` = 320x240; see "Picture quality"),
`TOTV_STREAM_PSP_SYSTEM` (`<cores>/system`: read-only directory that holds `PPSSPP/`, see "PSP"), `TOTV_STREAM_CHILD_LOG=<file>` (debug only: the child's stderr incl. the core's own log goes to this file, 0600; unset = /dev/null), `TOTV_STREAM_RW_TMP` (default `TOTV_STREAM_TMP`: where the
core's writable directory is made, **must be a size-limited tmpfs**), `TOTV_STREAM_PSP_CORE_OPTS` (`ppsspp_x=y;...`),
`TOTV_STREAM_ALLOW_NO_ISOLATION=1` (the component itself then accepts sessions without seccomp/landlock; development only).
There is no TCP API: only the Unix socket (created 0660).

## API (as the Totv's `server/games/stream-client.js` calls it)

All calls need `Authorization: Bearer <secret>`; the secret file is re-read on every call (rotation needs no restart) and
compared in constant time (SHA-256 of both sides). Errors: `{ "error": "<short code>" }`.

| Route | Result |
|---|---|
| `GET /v1/health` | `{ version, encoder: "x264", gpu: null \| { api, renderer, version }, cores: { snes: ["server_snes9x"], n64: ["server_mupen64plus_next"] }, isolation: { seccomp, landlock, netns } }`. `gpu` is non-null only when `retro-emu --gltest` could create a real EGL/GL context at start; GPU cores are listed only then. |
| `GET /v1/sessions` | `[{ id, state }]` |
| `POST /v1/sessions` | `201 { id }` (`id` = `sessionId`). 409 `busy` (1 session), 400 `bad_request`/`unknown_core`, 502 `rom_download_failed`/`rom_not_found`/`save_load_failed`/`emulator_failed`, 503 `isolation_unavailable` |
| `POST /v1/sessions/:id/signal` | `{type:"offer",sdp}` gives `{type:"answer",sdp}` (host candidate already inside, only `iceHost`); `{candidate}` gives `{ok:true}`; `{candidate:null}` is accepted and ignored. SDP limit 64 KB, never logged. |
| `GET /v1/sessions/:id` | `{ state, playedMs, stats }` (extra `stats`: counters and latency percentiles per stage) |
| `DELETE /v1/sessions/:id` | `{ ok, saved }` only after the save RAM was PUT to the Totv; 502 `save_failed` if that failed |

Decisions taken here (the spec left them open, all **proposals**):

- **Invalid or missing secret:** `401 {"error":"unauthorized"}`.
- **Session states:** `starting`, `waiting_signal`, `connecting`, `playing`, `disconnected`, `stopping`, `stopped`.
- **Orphans:** a session with no signal in 60 s is ended (with save); a session whose peer was lost for 30 s is ended.
- **ROM:** only `mediaId` (PSP: ISO/CSO/PBP up to 2 GiB, as a file, never in memory, see "PSP"). Downloaded by the parent from `serverUrl` (accepted only as `http://127.0.0.1:<port><path>`, no redirects followed, 64 MB max) with the session token, into a private temp directory (0700, remove at the end). A `.zip` is unpacked in memory (`rom.go`): first `.sfc`/`.smc` entry, max 8 MiB after unpacking (declared size and real size both limited), entry names never used as paths (zip-slip), written to the session's temp directory; invalid zip gives `rom_invalid`. There is no ROM cache.
- **Save:** SRAM only (PSP: a zip of the memory-stick save directory through the same two routes, see "PSP"). `GET /api/games/:itemId/sram` at start (404 = no save; any other failure refuses the session, so an old save is never overwritten by an empty one), `PUT /api/games/:itemId/sram?core=server_<core>&mediaId=<id>` every 30 s if changed and at stop. Save states (`retro_serialize`) are NOT implemented.
- `stateId` is accepted and ignored.
- The Totv's `normalizeCores` currently only reads platforms with `streamOnly`; `snes` is not one of them, so the Totv ignores the `cores.snes` that this component reports until the Totv decides how to expose it.

### Input packet (12 bytes, big-endian) and bit order (proposal, to be agreed with the clients)

```
byte 0     version  uint8 = 1
byte 1-2   seq      uint16
byte 3-6   buttons  uint32  bit i set = button i pressed
byte 7-10  axes     4 x int8 (left X, left Y, right X, right Y), unused by the SNES, used by the N64 (Y positive = down)
byte 11    port     uint8 0..3
```

`buttons`, bit 0 is the least significant bit, in the order of the spec list:

| bit | name | bit | name | bit | name |
|---|---|---|---|---|---|
| 0 | dpad_up | 6 | x | 12 | l3 |
| 1 | dpad_down | 7 | y | 13 | r3 |
| 2 | dpad_left | 8 | l1 | 14 | start |
| 3 | dpad_right | 9 | r1 | 15 | select |
| 4 | a | 10 | l2 | 16 | guide (ignored) |
| 5 | b | 11 | r2 | 17-31 | reserved, must be 0 |

`a/b/x/y` are the PHYSICAL positions of an Xbox-style pad, as in Android `KEYCODE_BUTTON_A/B/X/Y`: `a` = south, `b` = east,
`x` = west, `y` = north. On the SNES this means `a` = SNES B, `b` = SNES A, `x` = SNES Y, `y` = SNES X.
Packets are accepted only with `version == 1`, 12 bytes, `port <= 3`; a `seq` equal to or older than the last accepted
(16-bit wrap-around: `int16(seq - last) > 0`) is dropped and counted (`inputOldDropped`); malformed ones are counted (`inputBad`).
The DataChannel must be labelled `input` (any other label is closed).

### How the parent hands the controller to the emulator child (proposal)

The child is `retro-emu`, started with a **list of arguments** (core, ROM, SRAM, system dir, size; never through a shell),
an **empty environment** and only two pipes. The token never reaches it. Parent converts the packet to a 16-bit RetroPad
mask and writes a fixed 4-byte message `[1, mask_lo, mask_hi, port]` to the child's stdin when the mask changes (the child
keeps the state; the client's 16 ms resend therefore costs nothing). The child reads the pipe (non-blocking) right
before every `retro_run`, so input applied is at most one frame old. Video, audio, save RAM and errors come back on stdout
as `[type][len][payload]` frames (see the header of `emu/host.c`). Child `stderr` goes to `/dev/null`.

## Isolation: what is real

`isolation.seccomp` and `isolation.landlock` are `true` only when the check succeeds at start: the component runs
`retro-emu --selftest`, which applies the same sandbox to itself and then tries a forbidden `socket()`, `ptrace()` and an
`open("/proc/self/stat")` (which worked before). A flag is `true` only if socket/ptrace returned `EPERM` (seccomp) and the
open that worked before now returns `EACCES` (Landlock). Each session child also reports the flags it really got in its
hello message; with isolation missing and no `TOTV_STREAM_ALLOW_NO_ISOLATION=1` the session is refused
(`isolation_unavailable`, 503).

Order inside the child (`emu/host.c`): (1) ROM and save RAM are read into memory; (2) `unshare(CLONE_NEWUSER|CLONE_NEWNET)` and
all capabilities dropped; (3) `dlopen(core, RTLD_NOW)` and `retro_get_system_info`; (4) seccomp + Landlock; (5) `retro_init`,
`retro_load_game` with the ROM from memory, save RAM copied in from memory. Code of the core therefore only runs sandboxed; what runs
before is the dynamic loader and the core's ELF constructors (a malicious `.so` is outside this threat model: cores come from
the image, not from users). A core with `need_fullpath` (PSP) is NOT read into memory: it gets a directory that holds only the ROM, read-only, and its own writable
directory (see "PSP"); snes9x, mupen64plus-next and the test core do not need it.

The parent does not take the child's word for it. After the hello it reads `/proc/<pid>/status` and requires `Seccomp: 2` and
`NoNewPrivs: 1` (otherwise it kills the child and answers `isolation_unavailable`), and it compares `/proc/<pid>/ns/net` with its own
to see whether the netns is real. Landlock has no `/proc` field: it is proven by the child's flags plus `--selftest` (which does the
forbidden `open()` from inside) and by the probe core test.

Implemented: parent `PR_SET_DUMPABLE=0`; child `PR_SET_NO_NEW_PRIVS`; seccomp-bpf applied with TSYNC: wrong arch and the x32 ABI
(`nr & 0x40000000`) kill the process; `EPERM` for `socket`, `socketpair`, `connect`, `bind`, `listen`, `accept(4)`, `sendto/msg/mmsg`,
`recvfrom/msg/mmsg` (this is what keeps UDP out: Landlock's network rules cover TCP bind/connect only), `ptrace`,
`process_vm_readv/writev`, `io_uring_setup/enter/register`, `tkill`, `pidfd_send_signal`, `bpf`, `perf_event_open`, `userfaultfd`,
`mount`, `setns`, keyctl and module syscalls; `kill`, `tgkill` and `rt_tgsigqueueinfo` are allowed only with the process' own pid
(never the parent, a group or `-1`); Landlock filesystem (read-only library dirs, nothing else) and Landlock TCP bind/connect
(ABI >= 4); user + network namespace (empty netns) where the kernel allows unprivileged user namespaces, best effort: `netns` in
`/v1/health` is `true` only when the selftest proved it, and a Docker default seccomp profile that blocks `unshare` makes it `false`.

Not implemented: the seccomp filter is still a deny-list, not an allow-list. A minimal allow-list was evaluated and not done: a libretro
core is arbitrary C (snes9x pulls in libc, libm, libstdc++, `mmap`/`mprotect`/futex/clock/threads variants that differ by glibc and
core version), so a list that is safe for snes9x today would be a source of random `SIGSYS` kills for the next core, and there is
no coverage here beyond snes9x and the test core. With the namespaces, Landlock and the deny list above the residual risk is
kernel bugs in the allowed syscalls. GPU cores (N64) are covered by the "N64" section below (the sandbox exceptions are listed there). Per-thread caveat: Landlock restricts the
calling thread and its future children, so a core that created threads in a constructor would not be covered (snes9x does not).
ROM in tmpfs with `size`: left to the compose file (`TOTV_STREAM_TMP` on a tmpfs); the component does not check it.

## N64 (prototype 2): feasibility of the "heavy" platforms, measured on this PC

Question: can the same component run a core that needs a 3D GPU (libretro `SET_HW_RENDER`), inside the same sandbox, at 60 fps, with
the same H.264/Opus/WebRTC path? **Yes, for N64: 60.0 fps sustained, ~25% of one CPU core, ~10% of the GPU, +0 ms of latency with the
default readback.** The price is not performance, it is isolation: a GPU core puts the NVIDIA kernel driver inside the emulator's
reachable surface (see "Isolation with a GPU"). Everything below was measured on: Arch Linux, kernel 7.3, RTX 3060 (driver 620.70.06,
OpenGL 4.6 core context), 12 CPUs, a desktop session running at the same time (idle GPU util 0 to 1%, 932 MB used before the tests).
Test ROM: `<your-n64-rom>.z64` from outside the repository, read only, served from memory by the fake Totv, never copied here.

### Core choice

| Option | Decision |
|---|---|
| **mupen64plus-next (libretro) + GLideN64, OpenGL 3.3 core** (`libretro/mupen64plus-libretro-nx` at `15bada8`) | **chosen**. HLE RSP + GLideN64 renders on the GPU through OpenGL: cheap (the whole GPU work of a frame, scale + flip, is 0.08 ms of host time), accurate enough for a commercial N64 game (intro, title, 3D cutscenes checked by eye), dynarec (x86-64) for the CPU, one libretro `.so` (5.1 MB) with GL only needing what the host's EGL context gives. Core options are plain libretro variables. |
| mupen64plus-next + parallel-RDP (Vulkan) | not built (its submodule is not part of the shallow clone and it needs a Vulkan device negotiated by the host: `SET_HW_RENDER_CONTEXT_NEGOTIATION_INTERFACE`). It is the accurate (LLE, per-pixel) choice and would be the answer for games GLideN64 draws wrongly; it costs more GPU and needs a second host backend (Vulkan device creation, surfaceless). Not measured. |
| mupen64plus-next + angrylion (software RDP) | the only option with no GPU at all (it would remove the isolation problem). Selecting it in this host (which gave it no GL context) crashed with SIGSEGV right after the ROM header was read; not investigated, CPU cost not measured. Worth a separate test if the GPU exposure turns out to be unacceptable. |
| parallel-n64 | older, less maintained fork of the same family; no reason to prefer it. |

GL over Vulkan for the first step: the context is created with plain EGL on the NVIDIA device without a window system, the core's own
GL state tracker (glsm) already works, and `glBlitFramebuffer`+`glReadPixels` is all the host needs. Vulkan would add a loader,
device negotiation and layers inside the sandbox for no gain at this resolution.

Build of the core (outside the repository, nothing from it is committed): `git clone https://github.com/libretro/mupen64plus-libretro-nx`,
`make -j4` produces `mupen64plus_next_libretro.so`; the x86-64 dynarec needs `nasm`, which this machine did not have (no sudo): nasm 3.02 was built
from source into a private prefix and put in `PATH` for the build. Copy the `.so` to `cores/` (ignored by git). The core is GPL-2.0 (GLideN64 too):
internal prototype, check licence compatibility before any distribution.

### Headless GPU context in the host (`emu/glhost.h`)

- **No X11/Wayland/GBM**: `EGL_EXT_platform_device` (`eglQueryDevicesEXT`, first device that is a DRM render node) + a surfaceless context
  (`EGL_KHR_surfaceless_context`, 16x16 pbuffer fallback). `libEGL.so.1` is `dlopen()`ed, so `retro-emu` still builds and runs without any GL
  (SNES-only machines); GPU support is compiled in when the EGL/GL headers exist (`HAVE_EGL`, Makefile checks).
- The host creates an **OpenGL 4.6 core context** (a core asking for 3.3 core is accepted; asking for more, another profile or GLES is refused with
  `SET_HW_RENDER` = false and the session fails, nothing is downgraded silently), then warms the driver up (FBO, shader compile, draw, PBO map, `glFinish`).
- `get_current_framebuffer` returns an FBO owned by the host (RGBA8 + depth/stencil, the core's max geometry); `get_proc_address` is `eglGetProcAddress`.
  `context_reset` is called after `retro_load_game`, like RetroArch.
- Per frame (`gl_frame_end`): `glBlitFramebuffer` core FBO -> output-size FBO (640x480, linear filter, **vertical flip done in the blit** because the core
  declares `bottom_left_origin`), then `glReadPixels` BGRA into the frame buffer that `to_i420` already uses (no extra copy), then the same CPU conversion as SNES.
- A duplicated frame (the core passes NULL: a commercial N64 game is a 30 fps game with `FrameDuping`) re-sends the previous picture, so the stream is a steady 60 fps
  (the parent counts 60.0 frames/s; an identical frame costs x264 almost nothing).
- Core options: `GET_VARIABLE` answers from `v.<key>=<value>` pairs the parent passes, and from the defaults the core announces with `SET_VARIABLES`
  (without defaults GLideN64 crashed in `TextureCache::_addTexture`: its options were zero).

**Readback cost** (`glReadPixels`, 640x480 BGRA = 1.2 MB, measured inside the host and sent every 120 frames in message `T`; averages over the last window of a 30 s run):

| readback | blit | readback | latency effect |
|---|---|---|---|
| **sync** (default): `glReadPixels` into memory, waits for the frame | 0.08 ms | **1.30 ms** (native 320x240 internal), 1.53 ms (640x480 internal) | none: probe 47.9 ms p50 vs 47.2 ms for the software test core |
| `pbo`: async DMA into a PBO ring, the previous frame is mapped | 0.08 ms | 0.26 ms | **+17 ms** (one frame behind): probe 64.5 ms p50 |

PBO is cheaper for the CPU thread but costs a whole frame of latency, which matters more than 1 ms of stall at 60 fps, so **sync is the default**
(`TOTV_STREAM_READBACK=pbo` to compare). The CPU RGB to I420 conversion (640x480) takes another 1.6 ms; doing it in a shader would shrink the read
to 0.46 MB and remove it. Not done (the budget is not tight).

### Measurements (30 s of a commercial N64 game, `N64=1 make n64`; window 34 s, Start every 3 s + stick wiggle, real pion client + ffmpeg decode)

| | native 320x240, sync | native, PBO | internal 640x480, sync | internal 640x480, PBO |
|---|---|---|---|---|
| sustained frames from the core | **60.01 fps** | 59.87 fps | 60.02 fps | 59.99 fps |
| encoded / audio RTP | 60.0 fps / 50.0 pkt/s | 59.9 / 49.9 | 60.0 / 50.0 | 60.0 / 50.0 |
| frames dropped (server) | 0 | 0 | 0 | 0 |
| core `retro_run` (CPU, without blit/readback) | 1.71 ms avg (7.0 max of window) | 2.13 (8.4) | 1.69 | 2.16 |
| host frame total (run + convert + send) | 4.75 ms avg, 9.8 max | 4.13 | 4.98 | 4.11 |
| x264 encode (p50 / p95) | 0.94 / 1.53 ms | 1.01 / 1.67 | 0.93 / 1.52 | 0.95 / 1.58 |
| `retro-emu` CPU (all threads, % of one core) | 27% | 24% | 28% | 23% |
| component + test client process | ~40% of one core | 42% | 40% | 41% |
| `retro-emu` RSS | 322 to 341 MB | 327 to 346 | 322 to 341 | 327 to 346 |
| whole-GPU util (p50 / max, shared with the desktop; idle 0 to 1%) | 9 / 23% | 8 / 16% | 11 / 24% | 11 / 23% |
| GPU memory used (whole GPU; 932 MB before) | 958 MB | 958 | 964 | 964 |

The "component + test client" line is the Go process that also contains the pion **client** and the decoder's pipe, so it over-states the server. The 1 s
buckets of frames show 59 to 75 per second (short catch-up bursts after the host's pacing resyncs; the average is 60.0). The GL timer query (`GL_TIME_ELAPSED`
around `retro_run` + blit, `gpu` in the stage line) reads ~13 ms but spans the whole GL work of the frame **including the gaps where the GPU waits for the CPU**:
it is not GPU busy time; the busy time is what the whole-GPU util shows (+8 to +11 percentage points over the idle desktop). `nvidia-smi pmon` did not report per-process `sm%`.
Running a **real Chrome headless** (below) received 1800 decoded video frames in 30 s (`getVideoPlaybackQuality`), i.e. 60 fps, `connectionState: connected`.

Native vs 2x internal resolution made no measurable difference at this scale: the GPU is hardly loaded, and the stream is 640x480 in both cases (native is upscaled
by the blit). 2x gives a sharper picture, so `TOTV_STREAM_N64_RES=2x` is a free choice here (not necessarily on weaker GPUs).

**Latency, input to decoded picture** (same method as `make latency`, loopback, x264, ffmpeg software decode, 30 presses; `make gl`): `glcore` (GPU path, sync readback)
**p50 47.9 ms, p95 53.0 ms**; PBO readback 64.5 / 66.5 ms; the software `testcore` 47.2 / 48.9 ms. Not glass to glass, no Fire TV, no Wi-Fi (same caveats as above).
Latency with the real game was not measured (no deterministic input to pixel effect in a commercial N64 game's title/cutscenes); the stage `input->frame` of the server is 7 to 9 ms p50 (frame boundary).

**Captures** (real Chrome headless, `scripts/chrome-shot.mjs`, no mock): the game reached the N64 logo, a publisher logo, a commercial N64 game title screen
(`n64-frame.png`, opened and checked) and, with Start/A sent, a new game and the (unskippable) 3D intro cutscene with a character
(185 s run, stable 60 fps). Gameplay with the stick was **not** reached (the cutscene is long), so the stick is proved by the `glcore` test and by the packet
path, not yet by playing. The picture used to have a black frame inside the 640x480 video: that is the N64 video interface's overscan (GLideN64 draws only the VI's active area). Measured on the
decoded stream, the non-black bounding box (max channel > 24) was **x 38..613, y 23..451** (575x428 of 640x480), now **x 0..640, y 1..479** (title, logos, file select).
Fix: the core's own overscan crop, set by the component (see "N64 picture" below).

### N64 picture: overscan crop and widescreen

- `TOTV_STREAM_N64_OVERSCAN="l,r,t,b"` (default `19,14,11,14`; `off` disables): GLideN64 overscan crop, in pixels of the N64's 320x240 frame (core options
  `mupen64plus-EnableOverscan` and `mupen64plus-Overscan{Left,Right,Top,Bottom}`; the unit is 320x240 pixels whatever the internal resolution). The default is what a commercial N64 game needs;
  the remaining 287x215 area is 4:3 within 0.1%, so the picture is scaled up to the 640x480 stream without distortion. Border size depends on each game's VI setup: a game with
  a thinner border loses a little of its edge (use smaller numbers or `off`), one with a thicker border keeps a thin black rim. Invalid values fall back to the default.
- The aspect stays **4:3** (`mupen64plus-aspect` default); the Android client does aspect-fit on the 640x480 frame, so on 16:9 TVs the side bars are the only black left. Nothing is stretched.
- `TOTV_STREAM_N64_ASPECT=16:9` (or `16:9 adjusted`), **off by default**: asks GLideN64 for real widescreen rendering (`mupen64plus-aspect`). It is not a stretch, the game
  renders a wider view, but many games do not support it (the extra width can show geometry popping in, HUD/2D elements and cutscenes may be wrong or still 4:3). The stream size is
  global, so also set `TOTV_STREAM_WIDTH=848 TOTV_STREAM_HEIGHT=480` or the wide picture is squeezed into 640x480 (not tested with a real game; the SNES core would be affected by that size too).
- `TOTV_STREAM_N64_CORE_OPTS="mupen64plus-x=y;..."`: extra core options (only keys starting with `mupen64plus-`), for experiments. Test env `N64_CORE_OPTS` does the same in `TestRealN64`, which logs
  the content bounding box (`BBOX`) of each screenshot.

### Audio, timing, saves

- The core reports 60.0 fps (real NTSC hardware is within about 0.2% of that; the host paces by what the core says, so audio and video stay locked with the core's 44.1 kHz) and 44.1 kHz audio, resampled
  to 48 kHz for Opus by the existing linear resampler (prototype quality). Audio peak 31667 of 32767 in the first 10 s, 50.0 RTP packets/s in every run.
- Saves: the core exposes ONE `SAVE_RAM` buffer of **296960 bytes** = EEPROM 2 KiB + 4 controller paks 4 x 32 KiB + SRAM 32 KiB + FlashRAM 128 KiB, the same `S`/`GET`/`PUT .../sram`
  path as the SNES (the 2 MiB download limit and the `sram_size` check are enough). `TestRealN64Save` proved: the size is announced in the hello, a save the Totv already has is
  loaded into the live buffer, a pattern in pak 2 comes back byte for byte after the session, and the save is uploaded because the core formats pak 1 and the game writes its EEPROM header
  at boot. Not proved: a save made by actually playing and reloaded in a game (needs gameplay). Save states are still not implemented.
- ROM: `.z64/.n64/.v64`, zip allowed (first such entry, max 64 MiB after unpacking; the download limit of 64 MiB is also the cap for a plain ROM). Byte order is handled by the core.

### Buttons and axes for N64 (proposal; the 12-byte packet is unchanged)

The packet's 4 axes are `int8` (-128..127). The N64 stick physically spans about +-80: the host multiplies by 256 into the libretro `int16` (-32768..32512), the core applies a 15%
dead zone, rescales and maps to +-80, so **the int8 axis is enough** (127 steps for 80 positions). Axis Y is positive **down** (Android `AXIS_Y`, libretro). Left stick = control stick,
right stick = the four C buttons (core threshold 50%). Core option `alt-map=True` (independent C buttons) is set by the component; packet bit to N64:

| packet bit | N64 | | packet bit | N64 |
|---|---|---|---|---|
| a (south: Xbox A, DualShock cross) | A | | l2 | Z |
| x (west: Xbox X, DualShock square) | B | | r1 and r2 | R |
| b (east: circle) | C-Down | | l1 | L |
| y (north: triangle) | C-Up | | l3 / r3 | C-Left / C-Right |
| dpad | D-pad | | start | Start |
| right stick (axes 2,3) | C-Left/Right/Up/Down | | select, guide | unused |

How the clients get there (not implemented in this repository; Android and web only need to fill the existing packet):
- **Web (Gamepad API, `mapping: "standard"`, DualShock 4/5 and Xbox pads report it in Chrome)**: buttons 0,1,2,3 = a,b,x,y; 4,5 = l1,r1; 6,7 = l2,r2 (use `.pressed`, or value > 0.5); 8 = select, 9 = start, 10,11 = l3,r3;
  12..15 = dpad up/down/left/right; 16 = guide. `axes[0..3]` = left X, left Y, right X, right Y, each `round(v * 127)` clamped to -128..127. Chrome's DualShock names (Cross=0, Circle=1, Square=2, Triangle=3) already match.
- **Android**: `KEYCODE_BUTTON_A/B/X/Y`, `_L1/_R1/_L2/_R2/_THUMBL/_THUMBR/_START/_SELECT` for the buttons (the `a` = south convention of this README); sticks from `MotionEvent`
  `AXIS_X/AXIS_Y` (left) and `AXIS_Z/AXIS_RZ` (right on DualShock/Xbox in Android; some pads use `AXIS_RX/AXIS_RY`), triggers `AXIS_LTRIGGER/AXIS_RTRIGGER` or `AXIS_BRAKE/AXIS_GAS` > 0.5 as l2/r2.
  A remote or keyboard with only a D-pad should send the D-pad as the left stick too (+-127) for games that read only the analog stick.
- Digital pads and the Fire TV remote have no right stick: the buttons `b`,`y`,`l3`,`r3` are the C buttons.

### Isolation with a GPU: what is relaxed, why, what the parent checks

Order in the child (`emu/host.c`) for a GPU core: ROM and save to memory; `unshare(user+net)`, capabilities dropped; **EGL/GL context created and warmed up** (this opens the device nodes and loads the driver:
the descriptors and mappings exist before any filter); `dlopen` of the core; **seccomp + Landlock**; only then `retro_init`/`retro_load_game` (the core asks for its context, the host checks it is the one that was created).

Observed after the sandbox (child pid; **before** "GPU isolation (opção 2)" below closed the unneeded ones): fds were `/dev/nvidiactl` x4, `/dev/nvidia0` x12, `/dev/nvidia-modeset` x2, `/dev/dri/renderD128` x2 and the protocol pipe; now only `/dev/nvidiactl` x4 of the GPU nodes; **1 thread** (mupen64plus-next uses coroutines, the NVIDIA driver started no helper thread because
the host sets `__GL_THREADED_OPTIMIZATIONS=0`), `CapEff: 0`, `NoNewPrivs: 1`, `Seccomp: 2`, own network namespace.

Every relaxation, exactly:

| # | Relaxation | Why (evidence) | Risk |
|---|---|---|---|
| 1 | **No seccomp change.** `ioctl`, `mmap`, `mprotect`, `futex`, `memfd`, shared memory were never denied by the deny-list, so the GPU works under the same filter (socket/connect/ptrace/io_uring/kill/x32 still denied, flags 3 + netns). | the `glcore` and mupen sessions run with `Seccomp: 2` on every thread | `ioctl` on the already open GPU fds is unrestricted (no allow-list by request code, see below) |
| 2 | **Landlock**: one extra rule, `/dev/nvidiactl` with `READ_FILE|WRITE_FILE|IOCTL_DEV` (list in `TOTV_STREAM_GPU_DEVICES`, only `/dev/...` entries without `;` or `=` are passed). Nothing else under `/dev`, no `/dev/dri`, no `/dev/nvidia0`/`-uvm` after the sandbox. | Without it the picture is **silently black** while audio and fps look normal: `libnvidia-eglcore` re-opens `/dev/nvidiactl` lazily on first use of some GL path (6 `openat` returning `EACCES`, found with `gdb catch syscall openat`; the only other denied opens in a 20 s trace were the harmless `<sysdir>/Mupen64plus/{mupen64plus.ini,shaders}`). | the core can open more `/dev/nvidiactl` descriptors (new RM clients). The same ioctl surface is already reachable through the 4 descriptors that exist, so no new class of exposure, but more driver objects/contexts and GPU memory |
| 3 | The host (not the parent) sets `__GL_SHADER_DISK_CACHE=0` and `__GL_THREADED_OPTIMIZATIONS=0` in the child (the child still starts with an **empty** environment from the parent; the token never reaches it). | no shader cache writes into a filesystem Landlock forbids; no driver threads (a thread created before the sandbox is covered by seccomp TSYNC but **not** by Landlock, which is per-thread) | none |
| 4 | Pre-sandbox work grew: the dynamic loader plus the NVIDIA/glvnd libraries and the driver's initialisation run **before** the filters (they are trusted code, not the ROM-controlled core). | only way to have fds and mappings already open | a vulnerability in driver initialisation is outside the sandbox; the inputs are the host's own fixed parameters |
| 5 | Debug hooks `RETRO_EMU_LOG` and `RETRO_EMU_GLDEBUG` exist in the child; they only work if the env var is set, and the parent always starts the child with an empty environment. | debugging | none while the parent keeps `cmd.Env = {}` |

**What the parent checks** (kernel's view, not the child's word): `/proc/<pid>/status` `Seccomp: 2` and `NoNewPrivs: 1` for the process **and for every thread in `/proc/<pid>/task/*/status`**
(new in `verifyChildSandbox`), `/proc/<pid>/ns/net` different from its own, and the hello flags (bit 1 seccomp, 2 Landlock, 4 Landlock net, 8 netns, **16 GPU context created before the sandbox**, 32 private `/dev/shm` (PSP); a GPU session
needs 3 as before). For a person debugging by hand: `grep -E 'Seccomp|NoNewPrivs|Threads|CapEff' /proc/<pid>/status` (expect `2`, `1`, `1`, `0`), `ls -l /proc/<pid>/fd` (only the GPU nodes above, the pipe, `/dev/null`) and
`readlink /proc/<pid>/ns/net`. Landlock still has no `/proc` field: it is proved by `retro-emu --selftest` and by the fact that the same core cannot be loaded without rule 2 (it renders black).

**What stays without protection.** With a GPU core the emulator can talk to the NVIDIA kernel module through `ioctl`s on its open descriptors: that is a large, closed-source attack surface (and a way to hang or monopolise the GPU the desktop uses; the
existing 5 s "no video" watchdog ends the session but a driver hang can outlive it). The threat is a **hostile ROM exploiting the emulator** (cores are trusted code, ROMs are not). The seccomp deny-list cannot express "only the ioctls the driver needs" without
measuring them (NVIDIA RM `'F'`, UVM, DRM `'d'` request codes) and it was not done. Proposals, from cheapest to strongest:

1. an `ioctl` allow-list by request type, built from a recorded trace of real games (breaks silently on driver updates: test per driver version);
2. run the child as a dedicated unprivileged uid in a container whose cgroup device allow-list contains only `nvidiactl`, `nvidia0`, `nvidia-uvm`, `dri/renderD128` (what the compose file must add: `devices:`, never `privileged`);
3. a **separate graphics process** is not a real separation here (the core calls GL), so it does not help; what helps is a separate machine/VM: a microVM with virtio-gpu/Venus or GPU passthrough of a second GPU, with the component outside;
4. for N64 only: a software RDP (angrylion) needs no GPU at all; it was not measured.

### Tests

| Test | What it proves | ROM |
|---|---|---|
| `TestGLCoreEndToEnd` (`make gl`) | own test core `emu/testcore/glcore.c` draws an R/G/B triangle with OpenGL core 3.3: EGL (before sandbox) -> FBO -> blit -> readback -> I420 -> x264 -> WebRTC -> ffmpeg. Colors exact (pure R, G, B corners, background (0,0,64): no channel swap), RetroPad A (`b`) makes the whole picture white, left stick axes move the triangle (centroid x 332 -> 481 right, 143 left), flags `3|16`, every thread `Seccomp: 2` | none |
| `TestLatencyProbeGL` (`PROBE=1`, `make gl`) | input to decoded picture through the GPU path | none |
| `TestHostOptionsSanitized`, `TestN64ButtonMapping` | the option string only carries `/dev/..` nodes; mapping table, int8 axes round trip | none |
| `TestRealN64` (`N64=1 make n64`) | 30 s of a commercial N64 game: fps, stages, CPU, RSS, GPU, PNGs (`N64_OUT`), fails below 50 fps or if the picture stays black | external |
| `TestRealN64Save` (`N64=1`) | 296960-byte save round trip | external |
| `TestChromeServe` with `N64_CHROME=<s>` + `scripts/chrome-shot.mjs` | real Chrome headless receives the game (60 fps decoded) and takes PNGs | external |

`make test` (with `-race`) passes with all of the above skipped or run (the GL test runs when `retro-emu --gltest` finds a GPU, and skips otherwise).

### Verdict

**N64: viable, and cheap.** 60 fps sustained, 25% of one core in the emulator, ~10% of a mid-range GPU, 1 to 1.5 ms of readback, 1 ms of x264, no added latency with the sync readback, one libretro core, one small GL layer in the host.
The remaining N64 work is polish (gameplay checks of the stick, overscan crop, game compatibility list for GLideN64 vs parallel-RDP, audio resampler), not feasibility.

**What it said about PSP and PS2 before the PSP prototype** (estimates written at the N64 stage; the PSP ones are now superseded by the measured "PSP" section, the PS2 ones are still only estimates):
- *PSP* (PPSSPP libretro, OpenGL/Vulkan HW render, 480x272, usually 60 fps): the same host path works as is (a GL context created before the sandbox, FBO, blit, readback); GPU cost is of the same order as N64 at 1x
  and grows with the render scale. What is missing is not GPU: ISOs are hundreds of MB to ~1.8 GB (`need_fullpath`, this component downloads into a 64 MiB-limited tmpfs and loads ROMs from memory), saves are a directory tree
  (`SAVEDATA`) rather than one blob, and PPSSPP wants a few threads (JIT + render), which interacts with Landlock being per-thread (threads created after the sandbox inherit it; threads created by the driver before it do not).
- *PS2* (the libretro PCSX2 port, GL/Vulkan, EE/VU/GS JIT, BIOS and 4 GB images, memory cards as files): the host/GPU path is the same, but it is the heavy one: several busy threads (needs the 4+ strong cores
  and a threads-aware sandbox review), a user-supplied BIOS in the system dir, `need_fullpath` ISOs of 1 to 9 GB, per-game compatibility hacks, and a much larger GPU/driver footprint per session (so the GPU-exposure problem above is worse and
  one GPU gets shared by fewer sessions). It needs a real CPU+GPU benchmark before any promise; this prototype only proves that the plumbing (EGL context before the sandbox, readback, encoder, input with analog sticks) is not the obstacle.
- *Common to both*: the isolation story stops being "no hardware" and becomes "a GPU driver is reachable": decide between the container/cgroup device policy (cheap, uid separation) and a VM per untrusted session before offering these platforms to users other than the owner.
- NVENC (`h264_nvenc` through ffmpeg) was only benchmarked in isolation: 600 frames of 640x480 test video in 0.29 s wall (x264 ultrafast, 4 threads, same clip: 0.22 s wall, 0.77 s CPU), i.e. both are sub-millisecond per frame at this size, so NVENC is not worth the extra
  GPU-device exposure of the parent (which holds the Totv token) at 640x480. It would matter at 1080p+ for PS2-class resolutions; not integrated.

### Not done / not proved (N64)

Fire TV hardware decoder, Wi-Fi, glass-to-glass latency, gameplay with the stick in the real game, concurrent sessions (the GPU is shared, `MaxSessions` stays 1), Vulkan/parallel-RDP, angrylion, the Docker image with
EGL/driver libraries (`libegl-dev` and the NVIDIA container toolkit are needed; the image was not built), save made by playing, pak/rumble options, 4 controllers, GPU-hang recovery.

## PSP (prototype 3): PPSSPP on the same GPU path, measured on this PC

Branch `psp`, small commits by theme. Question: does the "heavy" plumbing built for the N64 carry a second GPU core that is different in
four ways (ROM is a file of up to ~1.8 GB, the save is a directory, the core needs a system directory, the core is multi-threaded)?
**Yes: PPSSPP boots and renders on the GPU inside the sandbox, 59.9 fps sustained, ~30% of one CPU core and no measurable GPU cost for a light
homebrew, with a real save round trip; it needed four sandbox changes, listed below, and one of them (a private `/dev/shm`) is new in kind.**
**What this did NOT show at first:** the test content was a homebrew (the PSPSDK `gu/cube` sample, BSD) and ISO/CSO images made from it. A commercial game
(a commercial PSP game, 341 MB ISO, from outside the repo, read only) was measured afterwards: see "A commercial game: the commercial PSP game". CPU/GPU numbers below are the homebrew's;
the the commercial PSP game ones are in that section.

### Measurements (`make psp`: `PSP=1 PSP_ROM=<file> go test -run TestRealPSP`; window 34 s, Cross every 3 s + stick wiggle, real pion client + ffmpeg decode)

Same method and machine as the N64 table (Arch, kernel 7.3, RTX 3060, driver 620.70.06, 12 CPUs, the desktop in use: **the GPU was already at 9% / 1036 MiB idle**, so "GPU util 9%" is the desktop). Content: `scripts/psp-homebrew/cube.c`
(PSPSDK `gu/cube`, rotating textured cube) as PBP (149 KB), as ISO (2.4 MB) and as CSO (1.0 MB); the "heavy" variant draws 400 cubes (4800 triangles) per frame plus ~4M iterations of float math (synthetic, not a game).

| | cube, native 480x272 | 2x (960x544) | 4x (1920x1088) | heavy (synthetic), native |
|---|---|---|---|---|
| sustained frames from the core | **59.93 fps** | 59.95 | 59.94 | 59.94 |
| encoded / audio RTP | 59.93 fps / 50.0 pkt/s | 59.95 / 50.0 | 59.94 / 50.0 | 59.94 / 50.0 |
| frames dropped (server) | 0 | 0 | 0 | 0 |
| 1 s buckets of frames | min 60 / p50 67 / max 76 | | | min 60 / p50 66 / max 75 |
| host: `retro_run` / blit / readback / convert / total | 0.29 / 0.08 / **1.59** / 3.24 / 5.20 ms (max 8.2) | 0.31 / 0.08 / 1.80 / 3.39 / 5.58 | 0.30 / 0.07 / **2.86** / 2.98 / 6.20 | 0.26 / 0.09 / 1.44 / 3.40 / 5.19 |
| GL timer (`gpu`) | 0.26 ms | 0.45 | 1.59 | 0.18 |
| x264 encode p50 / p95 | 1.20 / 1.57 ms | 1.21 / 1.56 | | 1.13 / 1.73 |
| `retro-emu` CPU (all 41 threads, % of one core) | **30%** | 31% | 35% | **50%** |
| `retro-emu` RSS | 231 to 239 MB | 231 | 231 | 247 |
| whole-GPU util p50 / max (idle desktop 9%) | 9 / 20% | 11 / 21% | 16 / 22% | 8 / 13% |
| GPU memory, whole GPU (1036 MiB idle) | 1060 MiB | 1087 | 1136 | 1060 |

Notes. `retro_run` is only 0.3 ms because PPSSPP's GL backend runs the PSP CPU/GE on its own **emu thread** (`NeedsSeparateEmuThread`): the host sees the thread-hand-off, the real CPU cost is the whole-process 30% (all 41 threads: PPSSPP's `ThreadManager` starts 12 compute threads in a 36-slot pool, plus the emu thread and helpers).
The readback grows with the render scale because the core's FBO is blitted to 848x480 either way and the GPU has more to finish before `glReadPixels` returns (1.6 to 2.9 ms), not because more bytes are read. The 2x and 4x columns were measured before `inflight_frames=No buffer` became the default. The 2x/4x/heavy numbers say that PPSSPP at 4x does not stress this GPU with this content; a real game with
render-to-texture effects, depth readbacks or many shaders will; shader-compile stutter on first use of an effect is likely (the driver cache is disabled by the host; the PPSSPP in-core cache is in the session's tmpfs and dies with it).
Real Chrome headless received the stream (`getVideoPlaybackQuality`: 869 frames in 14.5 s, `connected`) and the PNGs were opened and checked: the textured cube (the "PSPDEV.ORG" logo) is rendered correctly, no channel swap, no flip, full 848x480 frame (non-background box x 0..848, y 0..480),
`n64-frame.png` (decoded stream) and `psp-chrome.png` (page in Chrome, 640 px wide video element) at the time of writing.
Start time: **0.10 s** from `POST /v1/sessions` to the hello for the 149 KB PBP (download + core load + `retro_load_game`); a real ISO adds its download time.

**Latency, input to decoded picture** (`TestRealPSPLatency`, same method as `make gl`: Cross held turns the homebrew's whole background white, 30 presses, loopback, x264, ffmpeg software decode):

| setting | press p50 / p95 | release p50 / p95 |
|---|---|---|
| `inflight_frames=Up to 1` (core default) | 116.7 / 123.5 ms | 116.5 / 121.4 ms |
| **`inflight_frames=No buffer` (the component's default)** | **100.0 / 103.7 ms** | 99.7 / 101.3 ms |
| + `force_lag_sync=enabled` (Low-Latency CPU Timing), + `frame_duplication=disabled` | 99.9 / 106.3 and 100.0 / 106.4 ms | no gain |
| for comparison: N64 `glcore` / test core | 47.9 / 47.2 ms | |

So PSP costs **~50 ms more than the N64 path** (100 vs 48 ms). The pipeline (encode, wire, decode) is the same; the difference is inside the emulated machine and the core: the game reads the pad at the start of its frame, draws into the back buffer, the swap happens at the next vblank, and PPSSPP's emu thread presents it one hand-off later
(a real PSP has about two frames of its own). ~100 ms input-to-picture on loopback, plus the network and the TV decoder, is playable for most PSP games and not for rhythm/fighting ones; this is a property of the core, not of the stream.

### A commercial game: the commercial PSP game (USA), measured on this PC

A commercial PSP game, a 341 MB ISO read from outside the repo (never copied into it), served by the fake Totv as the session's ROM (start 0.29 s: the ISO is copied to the session's temp directory).
`make psp` with `PSP_KIND=game` (see below); the the commercial PSP game runs used the same sandbox (flags 63, 41 threads, all under the filter) as the homebrew.

**The "stuck at the title" report was the input, not the component.** The core was not frozen (the core's frame counter and the decoded frames both kept moving: 184 to 553 distinct frames
per 60-130 s, `child state=S`, CPU time growing). At the title the QA pressed Cross, and the cursor of the commercial PSP game's title is on **CONTINUE**, not NEW GAME: with no save the game calls `sceUtilitySavedataInitStart(LISTLOAD)`
("Save data not found: ms0:/PSP/SAVEDATA/<GAMEID>_DATA01" in the core's log) and PPSSPP draws its own dialog on a dimmed picture (grey bar with a square at the top, two white lines, **no text**), which closes with Circle (cancel), not Cross.
`padToRetroPSP` is right (Cross = RetroPad B, Circle = A: checked by `TestPSPButtonMapping`; Circle closed the dialog and Up + Cross chose NEW GAME in the real run). With Circle, Up, Cross from the title the game ran the intro cutscene
(scrolls with subtitles) and reached the "To those who seeketh earthend ... Sign" contract screen; no screen after it was reached (the contract needs a different input that was not found).
What was also found in the core's log (`TOTV_STREAM_CHILD_LOG=<file>`, new, debug only): PPSSPP **auto-installs the 3.73 firmware from the disc** (the commercial PSP game's UMD carries the PSAR updater, 18 MB, "279 files, 0 failed") into the session's
tmpfs directory `PSP/NAND/flash0` (about 20 MB of RAM per session, gone with the session); it does not need and does not touch anything outside the sandbox's two writable places. No EACCES/EPERM, no shader failure, no missing asset was logged.
`terminate called without an active exception` is printed once when the session is stopped (core teardown), not during play.

| the commercial PSP game, native 480x272, `inflight_frames=No buffer` | |
|---|---|
| sustained frames from the core (60 to 150 s runs) | **59.94 fps**, 1 s buckets min 60 / p50 66-67 / max 75-78, 0 frames dropped, audio 50.0 RTP packets/s |
| `retro-emu` CPU (all 41 threads) | **32 to 40%** of one core (intro cutscenes and title) |
| `retro-emu` RSS | 218 to 261 MB |
| whole GPU util p50 / max (desktop idle 9%) | 9-10% / 27-35%; GPU memory 1067-1081 MiB (1036 idle) |
| host stages (`retro_run` / blit / readback / convert / total) | 0.27 / 0.08-0.10 / 1.4-1.7 / 3.2 / 5.0-5.2 ms |
| x264 / frame to wire | 1.1-1.4 ms / 1.4-1.7 ms |
| input to decoded frame, `TestRealPSPLatency` with `PSP_KIND=game` (Cross opens the dialog = picture dims; Circle closes it) | press p50 251 / p95 269 ms, release p50 629 / p95 634 ms: **includes the game's own fade** (the savedata dialog and the title fade in/out), so it is an upper bound, not the pipeline's ~100 ms |
| game save | none written (no save dialog was completed; the game did not write under `PSP/SAVEDATA`) |

Not proved with the game: the actual gameplay (drum rhythm, a mission), a save by the commercial PSP game, the dialog text (the dialog shows no text; whether that is the HLE font/`ppge_atlas.zim` of this asset set or PPSSPP's own behaviour for an empty list is not established:
the 1.8 MB asset set was not swapped for the full one because the PPSSPP source tree is not on this PC any more), and longer than 150 s.

`TestRealPSP` / `TestRealPSPLatency` assume the homebrew (rotating cube on a clear colour, stick moves the cube, Cross turns the background white, `RUNS.BIN` counter). With a commercial ROM set **`PSP_KIND=game`**: the cube/stick/white/`RUNS.BIN` assertions are skipped
and replaced by "the picture moves" (at least 3 distinct decoded frames sampled every 250 ms and a final screen different from the first, plus no black picture) with the input of `PSP_DRIVE_BUTTONS` (comma list of `cross,circle,square,triangle,start,select,up,down,left,right,l,r`), `PSP_DRIVE_START`, `PSP_DRIVE_EVERY`, `PSP_DRIVE_PRESS_SECS`, `PSP_DRIVE_HOLD_MS`;
the homebrew case keeps its original rigor. `TestRealPSPDrive` is the light diagnostic version (hash of the picture every 250 ms, PNG every `PSP_SHOT_EVERY` s, child state and CPU time): `PSP=1 PSP_ROM=<file> PSP_DRIVE_BUTTONS=circle,up,cross go test -run TestRealPSPDrive -v`.
Example: `PSP=1 PSP_KIND=game PSP_ROM='<your-psp-iso>' PSP_DRIVE_BUTTONS=up,cross PSP_DRIVE_START=18 PSP_DRIVE_PRESS_SECS=4 PSP_SECS=60 go test -run 'TestRealPSP$' -v`.

### Isolation: what PSP relaxed (each one is new; seccomp itself is unchanged)

Flags in the hello for a PSP session: **63** = seccomp 1 + landlock 2 + landlock net 4 + netns 8 + GPU 16 + **private /dev/shm 32**. `/proc/<pid>/status` of the real core process: `Seccomp: 2`, `NoNewPrivs: 1`, `CapEff: 0`, **41 threads, all with `Seccomp: 2` and `NoNewPrivs: 1`** (the parent's `verifyChildSandbox` checks every thread at the hello, and `TestRealPSP` re-checks all of `/proc/<pid>/task/*/status` after 2 s of play: 41 of 41; PPSSPP's threads are all created after the filter).
Landlock is per thread and inherited by threads created after `restrict_self`, which is the case for all of PPSSPP's (the core creates them in `retro_init`/`retro_load_game`); there is no `/proc` field to prove it per thread, so this rests on that rule plus the filesystem probes of the test core, not on a per-thread measurement.
Open descriptors of the real core after the sandbox: `/dev/nvidiactl` x4, `/dev/nvidia0` x12, `/dev/nvidia-modeset` x2, `/dev/dri/renderD128` x2, `/dev/shm/ppsspp_0.ram (deleted)`, the ROM file, two `/dev/null`, the pipes. **No sockets** (see 4).

| # | Relaxation | Why (evidence) | Risk |
|---|---|---|---|
| 1 | **Landlock read-only on `<session>/rom`**, a directory that holds only the ROM (`need_fullpath`) | the core opens the ISO by path (a); the old code gave read access to the directory of the ROM, which for SNES holds the save too, now only for cores that need it, and in a directory of its own | the core can read the game it runs; nothing writable there. The ISO is a disk copy for the session (RAM if the directory is a tmpfs) |
| 2 | **Landlock read+write on ONE directory, `<rw>` (tmpfs)**: `READ_FILE, WRITE_FILE, READ_DIR, MAKE_DIR, MAKE_REG, REMOVE_FILE, REMOVE_DIR, TRUNCATE, REFER`; **no** `MAKE_SYM/SOCK/FIFO/CHAR/BLOCK`, no `EXECUTE` | the memory stick (saves) and PPSSPP's caches (b); the test core proves that `..` out of it, a symlink in it and a write next to the ROM fail | a hostile ROM can fill that tmpfs (no quota here: **`TOTV_STREAM_RW_TMP` must be a tmpfs with `size=`**); a save over the limits is refused at packing time and the old save stays. It can write arbitrary bytes into its own save (that is what a save is); the parent re-validates the zip names/limits only when packing |
| 3 | **Landlock read-only on `<system dir>`** (the 1.8 MB `PPSSPP/` assets) | PPSSPP reads its atlases and `compat.ini` through its VFS | none beyond reading public asset files; **a shared directory**: the same files for every session |
| 4 | **Private `/dev/shm`**: the child's own **mount namespace** (`unshare(CLONE_NEWNS)` with the user and net namespaces, mounts made `MS_PRIVATE`), a fresh `tmpfs` (`size=1g,nosuid,nodev,noexec`) on `/dev/shm`, and read/write Landlock on exactly that directory | **PPSSPP builds the PSP's RAM (and mirrors, ~390 MB) as a shared-memory file**: `shm_open("/ppsspp_N.ram")`, falling back to `/dev/shm/gc_mem.tmp` and `/tmp/gc_mem.tmp`. Without it the core fails in `MemArena::GrabMemSpace` (`Failed to grab memory space as a file ... Permission denied`, `Memory init failed`, core dumped: seen). The alternative, Landlock read/write on the host's `/dev/shm`, would hand the core every other program's shared memory (browser, audio): **rejected**. The test core proves that a marker file in the host's `/dev/shm` is invisible to the child | needs unprivileged user+mount namespaces (as the netns already did; without them the PSP session is **refused**, not degraded); the mounts happen before the filters (pre-sandbox work grew again), and `mount/umount2/setns/pivot_root/chroot` stay denied by seccomp afterwards; the tmpfs is up to 1 GiB of RAM per session |
| 5 | **The user's own uid/gid are mapped to themselves in the child's user namespace** (`uid_map`/`gid_map`, `setgroups` denied) | files created in a tmpfs mounted by a user namespace need a mapped fsuid/fsgid (without it: `EOVERFLOW` from `shm_open`, seen). Side effect found with `gdb catch syscall socket`: with a **mapped** uid the NVIDIA EGL driver (`libnvidia-eglcore` through `libdbus-1`) **connects to the system D-Bus** during initialisation and keeps the descriptor open; with the unmapped uid (N64, no `shm=`) the connection fails and no socket is left. A `write()`/`read()` on that descriptor would talk to the bus (seccomp denies `sendmsg/recvmsg/connect/socket`, not plain `write`) | **fixed**: the host closes every socket descriptor right before the sandbox (`close_sockets`), also for N64/SNES; the real-core test fails if the child holds any `socket:` descriptor. Without the fix this would have been a hole |
| 6 | **Threads and JIT**: 41 threads, the dynarec's executable memory. Nothing to relax in seccomp (`mmap/mprotect/futex/clone` were never denied) | the core runs | same class of exposure as the N64 dynarec; more threads = more places the filter must hold (it does: `Seccomp: 2` on all) |
| 7 | The core tries to **create a probe file next to the ROM** | it checks whether the game directory is writable | denied by Landlock (rule 1), PPSSPP carries on |

**Unchanged and still open** (see "Isolation with a GPU"): the NVIDIA kernel module is reachable through `ioctl` on the open descriptors, there is no `ioctl` allow-list, a hostile ROM can hang the GPU the desktop uses. PSP adds JIT code with a large host surface (ISO/CSO readers, ATRAC/MP3/FFmpeg decoders on untrusted data, savedata parsers) that the N64 core has less of.
**Not enabled by default in the core and blocked anyway**: PPSSPP's ad hoc/WLAN/UPnP features (all off by default; every `socket()` is denied, so they cannot work).

### Tests (PSP)

| Test | What it proves | Needs |
|---|---|---|
| `TestSaveZipRoundTripOnlySavedata`, `TestSaveZipEmptyAndBusy`, `TestSaveZipSkipsSymlinksAndLimits`, `TestUnzipSaveRejectsHostileZips` (13 hostile cases), `TestUnzipSaveSizeAndHeaderTricks` | safe zip/unzip (`savezip.go`): only `PSP/SAVEDATA`, zip-slip, symlinks (also an existing symlink in the destination), limits on files/entry/total/packed counted in real bytes, lying size, bomb, local/central name mismatch, nothing left behind on failure | nothing |
| `TestSaveZipAcceptedByTotvValidator` | what we pack passes the Totv's real `validateSaveZip` (memory and file forms) | `TOTV_SAVES_JS=<totv>/server/games/saves.js`, node |
| `TestPSPFullPathSessionAndSave` | **the `need_fullpath` path**: a 256 MiB ROM is served, the child's peak RSS is 3.5 MB, the core (`pspcore`, `bin/pspcore_libretro.so`: `need_fullpath`, no SAVE_RAM, directory save) reads it by path and cannot write next to it, cannot create `..`/symlinks, cannot read `/etc/passwd`, gets a **private** `/dev/shm` (a host marker file is invisible), writes its memory stick, the zip comes back with only `PSP/SAVEDATA`, a second session restores it (counter 1 to 2), the writable directory is gone after the session | nothing |
| `TestPSPRefusesHostileSaveAndZipROM`, `TestPSPROMExtAndHostOptions`, `TestPSPButtonMapping` | a zip-slip save refuses the session, a zipped ROM is refused, ROM sniffing (`CISO`, `\0PBP`, ISO), host options carry no `;`/`=`/`..` paths, overrides replace defaults, the button table | nothing |
| `TestRealPSP` (`make psp`) | 30 s of real PPSSPP: fps, stages, CPU, RSS, GPU, fds (fails on any socket), analog stick moves the picture, PNGs (`PSP_OUT`), the save zip with the homebrew's counter | `PSP=1 PSP_ROM=...`, `cores/ppsspp_libretro.so`, `cores/system/PPSSPP`, a GPU |
| `TestRealPSPLatency` | input to decoded picture (`PSP_KIND=game`: dim/restore probe for a commercial game) | same, ffmpeg |
| `TestRealPSPDrive` | a commercial game driven by a button schedule: distinct frames, PNGs, child state | `PSP=1 PSP_ROM=...` |
| `TestChromeServe` with `PSP_CHROME=<s>` and `CHROME_W=848 CHROME_H=480 scripts/chrome-shot.mjs` | real Chrome headless decodes the PSP stream, PNG | same, Chrome, Node |

`go vet` and `go test -race ./...` pass (all of the above that need nothing, and the real ones skipped without `PSP=1`); the N64 (`N64=1`) and SNES (`SNES=1`) real tests were re-run after the host changes and still pass (N64: 60.05 fps, flags 31, 296960-byte save round trip).

### Verdict

**PSP is viable on this architecture, with a caveat that only a real game can remove.** The sandbox, GL, readback, encoder and input paths carried PPSSPP with four relaxations (read-only ROM directory, one writable tmpfs directory, a private `/dev/shm`, mapped uid + closing inherited sockets), all documented above and none touching seccomp.
60 fps held in every configuration tried, the cost of the emulator (30% of a core for a light homebrew, 50% for a synthetic heavy one, 231 to 247 MB RSS) and of the stream (readback 1.6 ms, x264 1.2 ms) is small on this 12-core PC. Directory saves work as one validated zip against the Totv's own validator.
What is **not** known: how a real game loads the CPU (the PSP's 333 MHz MIPS runs at JIT speed, games that saturate it are the ones that cost; PPSSPP's own compatibility list and speed vary per game), GPU effects at 2x+, long-run stability and memory growth, and the first-use shader-compile stutter. Latency is the one measured disadvantage: **~100 ms** input to picture on loopback (N64 ~48 ms).

**What it says about PS2** (an inference, not a measurement; there is no PS2 core or BIOS here):
- The plumbing for PS2 is now **proved in kind**: GL core context, readback, a `need_fullpath` multi-GB image that is never in memory, a directory-or-file save packed as a zip, extra read-only/read-write directories in Landlock, a private shm/tmpfs, many threads under one filter, analog sticks. PS2 would reuse all of it (the BIOS goes in a read-only system directory like the PPSSPP assets; memory cards are files, `zipSave` with a 16 MiB limit is already in the Totv's `platforms.js`).
- What does **not** carry over is cost and policy: the PS2 core (EE/VU/IOP JIT plus a GS renderer) is far heavier than PPSSPP on the CPU side (several busy threads, typically wanting 4+ strong cores and 1 to 2 GB of RAM per session, unmeasured), 4 to 9 GB images make the "ROM copy on tmpfs" idea impossible (the ROM directory must be disk-backed or the image served by range/VFS, and the 4 GB `.cue` bundle limit in the Totv is open), the per-game compatibility burden is much larger, and the GPU/driver footprint per session grows (the GPU exposure problem of "Isolation with a GPU" is worse, and one RTX 3060 shared with the desktop would carry fewer sessions).
- The new finding that matters for PS2: **cores that need a mapped user namespace or a shm file bring the driver's side effects with them** (the D-Bus socket), so every new heavy core needs the same descriptor/`/proc` audit of the running child (`ls -l /proc/<pid>/fd`, `grep Seccomp /proc/<pid>/task/*/status`) before being trusted. The plan stays: container/cgroup device policy now, a VM per untrusted session before offering heavy platforms beyond the owner.

### Not done / not proved (PSP)

- **Commercial game: only the commercial PSP game, only up to its title/intro/contract screens** (see "A commercial game"): CPU/GPU/RAM of real gameplay, compatibility, long runs (30 min), memory growth, shader-compile stutter, audio sync under load (audio was 50.0 RTP packets/s in every run, but the content is a silent cube).
- A real multi-GB ISO under the 60 s start window (only 256 MiB through the test core, 149 KB/2.4 MB/1.0 MB through the real one); a CSO/ISO bigger than RAM-backed `TOTV_STREAM_TMP`; ROM cache; HTTP `Range`/VFS instead of a full copy.
- Saves made by a game's own save dialog (`sceUtility`), save states, auto-save during play proven only for the "files unchanged for 2 s" rule with the test core and the homebrew (the periodic path ran with a 5 s tick in `TestRealPSP`, the homebrew writes once at start), upload streaming, saves above 64 MiB.
- The save zip against a **running** Totv (only the validator function and a fake Totv); the Totv's POST timeout for a long start.
- Vulkan backend, 2x+ on a real game, other GPUs, Mesa/Intel/AMD (the context code is vendor-neutral; the D-Bus finding and the descriptor audit are NVIDIA-specific observations), PPSSPP ad hoc, `TOTV_STREAM_RW_TMP` as a real size-limited tmpfs in Docker (compose file not updated), unprivileged user namespaces being blocked by Docker's default seccomp profile (the PSP session is refused then, by design).
- Landlock per thread for PPSSPP's 41 threads (argued from inheritance, not measured).

## PS2 (prototype 4): the PCSX2 libretro core on the same path, WITHOUT a BIOS or a game on this PC

Branch `ps2` (local, from the PSP work). Question: does the heavy-platform plumbing (GL context before the sandbox, `need_fullpath` image, private `/dev/shm`, directory save as a zip, cgroup per
session) carry a third GPU core, the biggest one, whose BIOS the project can not ship and whose images are 4-9 GB? **Answer so far: everything around the game is proved with a synthetic
BIOS and a test core; the real core loads, initialises and runs under the full sandbox; NO PS2 GAME HAS EVER RUN** (the machine holds only a PS1 BIOS and no PS2 image). The owner has to provide
a BIOS dump of their own console and an image to go further; the commands are at the end of this section.

### Which core (checked on 2026-11-23)

| Candidate | Finding |
|---|---|
| `libretro/LRPS2` (the name the task gave) | **does not exist any more**: `git ls-remote` asks for credentials (GitHub's answer for a missing repository) and the REST API returns 404 |
| `libretro/parallel-ps2` (**chosen**) | the live repository of the libretro org with a PCSX2-derived core: its README is titled "LRPS2", `COPYING.GPLv3`, last push 2026-11-22. Pinned at **77e2e0d86cac1b3dd54465d2547b0e7246b9ec3b**. Its headline work is an AArch64 port; on x86-64 it is the x86 PCSX2 recompilers with an OpenGL, a (not built here) Vulkan/paraLLEl-GS and two software renderers. `pcsx2/Memory.c` etc. are mostly LGPL-3.0-or-later, some GPL-3.0-or-later, a few GPL-2.0-or-later, MIT and BSD-3-Clause files (SPDX tags counted in `pcsx2/`), VIXL (BSD-3-Clause) vendored; the whole is **GPL-3.0** (COPYING). Compatible with this project's GPL-3.0-or-later host; **the exact licence still has to be confirmed by the owner (NOTICE.md)** |
| `jpd002/Play-` (Play!) | `License.txt` is a BSD-style licence (copyright line, two conditions, disclaimer; permissive, GPL-compatible). `libretro/libretro-play` does not exist and `libretro/Play-` redirects to `libretro-mirrors/Play-`, last push 2020-08-17: **not built, not evaluated beyond reading licence and repository state**; kept as the fallback if PCSX2 turns out not to work with a real game |

Build (no cmake, no external library: the core links only libc and libm, libstdc++ is static): `make -j4 IS_X86=1 HAVE_VULKAN=0 HAVE_OPENGL=1` in the clone, nice 15, **about 2 minutes**, `.so` of 8.5 MB, GCC 16.2.
**Pitfall found:** the Makefile sets `IS_X86` from `uname -p`, which prints `unknown` on this distribution, so `-msse4.1` is missing and the GS files fail with "target specific option mismatch";
`IS_X86=1` on the command line fixes it (also set in `scripts/fetch-cores.sh pcsx2`, which was written but **not run in the Debian container**: the `.so` of these tests was built on the host).
`HAVE_VULKAN=0`: the host only creates the OpenGL 4.6 core context, so Vulkan and paraLLEl-GS are not offered (`pcsx2_renderer=OpenGL` is set by the component).

### What the core asks of its host (from its source and from running it)

- System directory `<system>/pcsx2/`: `bios/` (the BIOS), `memcards/` (`Mcd001.ps2`, `Mcd002.ps2`, **8650752 bytes each**, created blank = all 0xFF when the VM starts), `cache/` (GL program binary cache `gl_programs.bin`, 2.9 MB after the first run), `cheats*`. It also writes **next to the BIOS** (`.mec`, `.nvm`): the BIOS directory must be writable, which is why the component copies the BIOS per session (below).
- `need_fullpath` (the image is opened by path; the core's own readers do ISO, CHD, CSO, ZSO).
- A **4 GiB fastmem area** (`Fastmem area: 0x7f..-0x7f..`, sparse) built on a shared-memory file: **without the private `/dev/shm` the core fails** (`Could not set up the host memory maps`, `(CodeReserve) Failed to allocate ...`) **and then segfaults** (seen). With `shm=1` it initialises. Touched shm at the BIOS idle: 261 MB.
- JIT: anonymous RWX mappings (EE, IOP, VU recompilers). Neither the seccomp filter nor Landlock restricts `mmap/mprotect` with `PROT_EXEC` of anonymous memory, so nothing had to be relaxed (see "Isolation").
- Pad: `retro_input_state` with `RETRO_DEVICE_JOYPAD` + `ID_JOYPAD_MASK`, the pressure of the face buttons through `RETRO_DEVICE_ANALOG`/`INDEX_ANALOG_BUTTON` (the host answers 0 there and the core then uses the digital bit as a full press), both analog sticks.

### BIOS: how the Totv/the user provides it (the component NEVER distributes one)

- The operator puts dumps of **their own console** (`scph*.bin`, optionally `.rom1/.rom2/.erom/.nvm/.mec` beside it) in a directory and sets **`TOTV_STREAM_PS2_BIOS_DIR`** (a read-only directory is fine). The component reads it; **the core can never read it** (not in the Landlock rules).
- Per session the parent picks the first valid file (name order), and **copies** it with its companions into the session's writable directory `<rw>/pcsx2/bios/` (the core writes `.nvm/.mec` there; the copy is `O_EXCL`, 0600, `O_NOFOLLOW` on the source, 16 MiB at most), sets `v.pcsx2_bios=<name>` (a plain name: no `/ ; = ..`), and the copy disappears with the session. The operator's directory is not touched (test: listing and mtimes identical before and after).
- **Validity** is the core's own test, repeated in Go (`ps2BiosCheck`, from `pcsx2/ps2/BiosTools.c`): size 1..16 MiB, a `RESET` entry in the ROMDIR table at the start, and a `ROMVER` entry whose 14 bytes are text. No directory, an empty one or only companions: **`bios_missing`**; files that are there but none valid (a PS1 BIOS, a truncated dump, no `ROMVER`): **`bios_invalid`**. Symlinks are skipped.
- **The session is refused before a byte of the image is downloaded**: `POST /v1/sessions` -> 502 `{"error":"bios_missing"}` / `{"error":"bios_invalid"}`. `/v1/health` has `"ps2": {"bios": false, "reason": "bios_missing", "bios_dir_set": false, "bios_files": 0, "bios_valid": 0, "max_iso_mb": 9216, "big_tmp": "..."}` (re-read on every request; no file name is ever reported); `cores.ps2` is listed only when the BIOS is valid. The start of the component logs a WARNING when the BIOS is missing.
- Proved against the REAL PS1 BIOS of this machine (an owner-supplied dump kept outside the repository, read-only, never copied into it: the test reads it into a temporary directory): the component answers `bios_invalid` (`TestRealPS2NoBIOSAndPS1BIOS`), and **the real core says the same when it is given that file** (`Could not find any valid PS2 BIOS File in ".../pcsx2/bios"`, game load fails, exit 2): the component's check and the core's agree.

### The image: by path, up to 9 GiB, on DISK, read-only for the core

- Same mechanism as the PSP (`rom/` directory with only the image, Landlock read-only on it, the host never reads it into memory; the parent sniffs `MComprHD`/`CISO`/`ZISO` to name the file `.chd/.cso/.zso`, anything else `.iso`; zip/gzip are refused as `rom_invalid`).
- Ceiling `TOTV_STREAM_PS2_MAX_ISO_MB` (default **9216**); `Content-Length` over it is refused before writing; `statfs` of the destination before the first byte (`no_space`, needs size + 64 MiB); `POST /v1/sessions` may wait up to **10 minutes** for the copy (the Totv's client must allow it; the PSP's was 60 s). The `emu/host.c` size check went from 4 GiB to 16 GiB (the component's own ceiling decides).
- **Where it goes:** a 9 GiB image does not fit in RAM next to the emulator, so the session directory (`ses-N/`: `rom/`) of a PS2 session is created in **`TOTV_STREAM_BIG_TMP`** (default `TOTV_STREAM_TMP`), which must be a **disk** directory; the writable directory (BIOS copy, cards, shader cache; quota 512 MiB) stays in `TOTV_STREAM_RW_TMP`, the size-limited tmpfs, as for the PSP. At start the component warns when `BIG_TMP` is a tmpfs smaller than ceiling + 4 GiB. Orphan `ses-N` directories in it are removed at start; the directory is removed at the end of every session (tested, also for refused sessions).
- **Read-in-place instead of a copy (Range/fd sharing): evaluated, not done.** The core opens a path under Landlock with no network; the only ways are a hard link/reflink of a file the Totv already keeps on the same filesystem (needs the Totv to hand over a path, a change of contract and trust) or a FUSE/NBD mount (needs privileges). A copy costs ~2.6 s for 5 GiB on this NVMe (page cache; a cold 8 GB disc takes what the disk takes) and disk space per session. Kept as an evolution.
- Tested with a **5 GiB sparse image** (`PS2_BIG_GB=5`): up in 2.6 s, child peak RSS 3.5-3.9 MB, the core read its head and tail (so a size above 4 GiB is handled everywhere), the session directory is gone afterwards; the default test uses 1.5 GiB.

### Saves: the memory cards as a zip (`savezip.go`, `ps2.go`)

- Package = a classic zip with **exactly** `pcsx2/memcards/Mcd001.ps2` and `pcsx2/memcards/Mcd002.ps2` (names fixed, nothing else is ever packed or restored: not the BIOS, not `.nvm/.mec`, not the shader cache; other names are ignored on the way in, like names outside the prefix). Same zip rules as the PSP (`PUT/GET /api/games/:itemId/sram`, no ZIP64, no symlinks, no `..`, ...); limits: 72 MiB per card, 80 MiB unpacked, 64 MiB packed, 16 entries counted (ignored ones included). Implemented as `saveLimits.Only` (an exact-name allow-list; the PSP leaves it nil: unchanged).
- A session always creates the two blank cards, so the first stop uploads a **~17 KB zip** (8.6 MB of 0xFF compress to nothing); a game that saves makes it bigger. The next session restores both files before the core starts (checked: counter written into the card by the test core goes 1 -> 2 across sessions).
- **What the Totv needs** (`server/games/saves.js`, not touched here): a `zipSave` type for platform `ps2` whose accepted names are those two files below `pcsx2/memcards/` (anything else refused or left out), with the PSP's limits adjusted (an entry up to 72 MiB, packed 64 MiB). The saves of an in-game save dialog and of the BIOS browser were not exercised (no game, no BIOS).

### Controller

The PS2 pad is the RetroPad one to one (`pcsx2/PAD/PAD.c`): `a/b/x/y` -> Cross/Circle/Square/Triangle (RetroPad B/A/Y/X), `l1/r1` -> L1/R1, `l2/r2` -> L2/R2, `l3/r3`, `start`, `select`, d-pad; `guide` ignored. It is exactly the default table (`padToRetroPS2 = padToRetro`).
**Both analog sticks are covered by the 12-byte packet**: its four int8 axes are left X, left Y, **right X, right Y**, handed to the core as `RETRO_DEVICE_INDEX_ANALOG_LEFT` and `..._RIGHT` (x256, so 127 -> 32512, -128 -> -32768). `pcsx2_analog_mode1/2=enabled` starts the pad in analog mode (the packet has no ANALOG button bit; a game that expects the user to press it would otherwise stay digital). Proved end to end (`TestPS2InputReachesTheCoreBothSticks`): over a real WebRTC DataChannel each of the 16 buttons arrives as the expected RetroPad bit and `{127,-128,64,-64}` arrives as `{32512,-32768,16384,-16384}`, the right stick alone with two buttons included. Not proved: that a real game reads them (no game).

### Isolation: the real core under the full sandbox (flags 1023)

`sandboxFlags=1023` for the real core through the component: seccomp (1), Landlock (2), Landlock net (4), netns (8), GPU context before the sandbox (16), private `/dev/shm` (32), Landlock signal scope (64), own IPC namespace (128), GPU descriptors closed (256), own PID namespace (512). `/proc/<pid>`: **3 threads, all `Seccomp: 2` + `NoNewPrivs: 1`, `CapEff: 0`** at the BIOS idle (a game may create more threads: the core also has an MTVU and a software-renderer thread pool; `pids.max` stays 256 per session; **not measured with a game**).

| Item | Relaxed? |
|---|---|
| seccomp deny-list | **no change**. The JIT (anonymous `mmap` with `PROT_EXEC`, `mprotect`), SIGSEGV handlers for fastmem, threads: all work as they are (`fork`/`clone` without `CLONE_THREAD`, sockets, `ptrace`, `unshare`, `clone3`... stay denied). The code reserve (the RWX area of the recompilers) was allocated without any `CodeReserve` error and the BIOS loop ran for 20 s (it is not proved that every recompiler emitted code: the loop is one block; the test core executes a written RWX page and an `mprotect` to RX, which is the pattern) |
| Landlock | **no change** except what the PSP already had: read-only on the image directory, read/write on ONE directory. For PS2 that directory is also the **system directory** (`GET_SYSTEM_DIRECTORY` = `GET_SAVE_DIRECTORY` = `<rw>`, because PCSX2 keeps `bios/`, `memcards/`, `cache/` under `<system>/pcsx2/`); there is no read-only assets directory. `EXECUTE` is still denied (no exec of files). The operator's BIOS directory is NOT readable by the core |
| private `/dev/shm` | the PSP mechanism (own mount namespace + `tmpfs`), **size per core**: new host option `shmmb=` (PSP stays 512 MiB, PS2 **1024 MiB**; 261 MB touched at the BIOS idle) |
| cgroup memory | **4096 MiB per PS2 session** (`coreInfo.memMB`, never below an operator's higher `TOTV_STREAM_SESSION_MEM_MB`) instead of 3072: the idle core is at 790 MB RSS (245 MB anonymous + 261 MB shm + driver); a game adds the GS targets and texture cache (1-2 GiB is what PCSX2 users report; **not measured**). Proved enforced: `TEST_CGROUP=require` run of the real core through `systemd-run --user --scope` read back `memory.max=4096M` and passed |
| `TOTV_STREAM_RW_TMP` | unchanged rule (a size-limited tmpfs, quota 512 MiB and 20000 entries, now counting the 4 MiB BIOS copy and the cards' **logical** size, ~21.5 MiB at rest); the BIG_TMP is a disk directory |
| uid / user namespace | unchanged: the PS2 session needs unprivileged user+mount namespaces as the PSP does (refused otherwise) |

What stays open for PS2 specifically: a JIT means a W+X mapping in the core's process by nature; a hostile BIOS/image is parsed by the ISO/CHD/CSO readers and the BIOS loader with the full surface of PCSX2 (HLE, IOP modules, CDVD, SPU2, DEV9 -- DEV9 is networking code: the core's own network stack is dead in the sandbox, no sockets); the NVIDIA ioctl surface as for N64/PSP.

### Measured on this PC (synthetic BIOS: a valid ROMDIR + `b .` loop written by `syntheticPS2BIOS`, a 3 MB junk image; **nothing is drawn, the numbers are for a core that runs an idle loop**)

| Command | Result |
|---|---|
| `PS2=1 go test -run 'TestRealPS2$' -v .` (the real core through the component) | session up in **5.6 s**; hello `59.94 fps`, `48000 Hz`; flags **1023**; 3 threads all filtered; VmRSS **794 MB** (anon 254 MB, shm 261 MB); `audioPackets` 720 in 12 s (the core runs frames: 60 Hz); `retro_run` avg **1.24 ms** (max 3.1) -- meaningless for a game, the EE is spinning in an idle-skipped loop; `framesIn=0` (the BIOS draws nothing; no `V` messages: expected); GL log lines from the core: `Using hardware for point expansion...`, `1 program binary formats supported by driver`, `GS: pool prewarmed, 13 target sizes, 11 MB`, `Resuming...`. Stop: `saved=true`, package **17198 bytes** zipped, two cards of 8650752 bytes, blank (0xFF); a second session restores them |
| same, `TEST_CGROUP=require` | passed (per-session cgroup read back) |
| `PS2=1 PS2_PS1_BIOS=<ps1 dump> go test -run TestRealPS2NoBIOSAndPS1BIOS` | no BIOS directory -> 502 `bios_missing` + `health.ps2.bios=false`; PS1 BIOS -> 502 `bios_invalid`; nothing left in the big tmp |
| the same loop through `retro-emu` directly | without `shm=1`: segfault; with it: runs; real BIOS absent: the core logs `Could not find any valid PS2 BIOS File` and `retro_load_game` fails |
| `make test` (`go vet` + `go test -race -count=1 ./...`) | passes (48.6 s) with all new PS2 tests |
| re-run of the older real tests after the host changes | SNES (`TestSNESKeepsStreaming`, `TestRealSNES`) pass; N64 `TestRealN64`: 60.01 fps, flags 991; PSP `TestRealPSP` with a commercial ISO: 59.93-59.96 fps, flags 1023, 41 threads all filtered |

### Tests (PS2)

| Test | What it proves | Needs |
|---|---|---|
| `TestPS2BiosScan` | 12 cases: no/empty/missing dir, companions only, a PS1 BIOS, no `RESET`, no `ROMVER`, non-text `ROMVER`, too small, valid, invalid first + valid second, symlink skipped; companions picked up | nothing |
| `TestPS2HealthAndRefusals` | `health.ps2` in the three states, the core is offered only with a valid BIOS, `bios_missing`/`bios_invalid` before any download, no file names leaked, nothing left in the big tmp | nothing |
| `TestPS2FullPathBIOSAndCards` (`PS2_BIG_GB=5`) | the whole path with `ps2core` (need_fullpath, RWX JIT-like page, shm, BIOS copy writable, cards): 1.5 GiB/5 GiB image on the disk tmp, peak RSS < 4 MB, BIOS copy and companion present and no foreign file, the core writes `.nvm` next to its copy, the operator's directory unchanged, zip with exactly the two cards, counter 1 -> 2 | nothing (disk space) |
| `TestPS2RefusesHostileSaveAndImage` | `..` name refuses the package; other names ignored and not extracted; zip image refused; ceiling; `no_space` | nothing |
| `TestPS2QuotaEndsAFillingCore` | a core that fills its directory is ended and uploads nothing | nothing |
| `TestPS2InputReachesTheCoreBothSticks` | 16 buttons + both sticks end to end over WebRTC | nothing |
| `TestPS2ButtonMapping`, `TestPS2ROMExtAndHostOptions`, `TestPS2CgroupMemory`, `TestPS2BigTmpWarning`, `TestPS2SaveZipOnlyTheCards` | mapping written out from the core's source, image sniffing, option sanitising (a BIOS name with `; = / ..` never reaches the host, an override can not replace `pcsx2_bios` or the shared cards), memory per core, big-tmp rules, the two-card zip | nothing |
| `TestRealPS2` (`make ps2`) | synthetic mode (no `PS2_ROM`): see the table above; real mode below | `PS2=1`, `cores/pcsx2_libretro.so`, a GPU |
| `TestRealPS2NoBIOSAndPS1BIOS` | the error paths with the real core installed | same (+ `PS2_PS1_BIOS=<file>` for the invalid half) |

**The test the owner runs with their own files:** `PS2=1 PS2_ROM=<image> PS2_BIOS_DIR=<dir with your BIOS> PS2_OUT=<png dir> make ps2` (optional `PS2_SECS`, `PS2_DRIVE_BUTTONS=start,cross`, `PS2_DRIVE_START`, `PS2_DRIVE_EVERY`, `PS2_CORE_OPTS`). Real mode decodes the stream, takes a PNG every 10 s, counts distinct decoded frames, checks audio, threads, RSS, the zip of the cards and a second session restoring them.

### Not proved (honest list)

- **A game.** Not one PS2 frame was drawn. No frame rate, no CPU/GPU/RAM under load (the idle numbers above are not an estimate of a game), no latency (the PSP needed 100 ms input to picture, the PS2 has the same core-side two-frame pipeline plus the EE/GS threads: expect the same or worse; unmeasured), no 30-minute run, no memory growth, no shader-compile stutter (the program cache is rebuilt in every session because the cache lives in the session's tmpfs: **a shared read-only cache across sessions is an evolution**).
- **That the OpenGL renderer works for a real game** (the core created its GL objects and pre-warmed its pools on the host's context; whether PCSX2's GL backend renders correctly on this driver is unknown), the per-game hacks, the GameDB (12807 entries loaded).
- A BIOS other than the synthetic one; BIOS `.rom1/.rom2/.erom` handling (copied, not exercised by the core); the fast-boot path (needs a valid disc); CHD/CSO/ZSO readers; images of 4-9 GB through the real core (5 GiB only through the test core); a dual-layer DVD.
- The memory cards as a game uses them (formatted, with saves), the BIOS memory-card browser, save states.
- The Totv side (`zipSave` for ps2, the 10-minute POST, `cores.ps2` / `health.ps2` handling) and Docker: `scripts/fetch-cores.sh pcsx2` was not run in the container, the compose example has no BIG_TMP volume nor a BIOS mount (needs: a disk volume at `TOTV_STREAM_BIG_TMP`, a read-only mount at `TOTV_STREAM_PS2_BIOS_DIR`, `mem_limit` of at least 6 GiB), `docker/seccomp-totv.json` was not re-checked.
- Vulkan/paraLLEl-GS, the software renderers (not selected), upscaling above 1x, multitap/second pad, USB/DEV9/HDD (DEV9 logs `open r+: DEV9hdd.raw` at start, the file does not exist and nothing was created; the core has no network: sockets are denied), other GPUs, Mesa.
- Whether the host's 640x480 stream size (NTSC 4:3 frame is 640x448, PAL 640x512; the host scales the whole frame) is right for 16:9 games: not examined.


## Picture quality (branch `perf-qualidade`): resolution and CRF, measured before and after

What changed (all of it can be overridden by environment, and a session can ask for the old picture with `"quality":"safe"`):

| | before (= `safe`) | after (`high`, the default) |
|---|---|---|
| N64 | core 320x240 (`TOTV_STREAM_N64_RES=2x` for 640x480), stream 640x480 | core 640x480 (`2x`; `native` and `3x` = 960x720 selectable), stream **960x720** |
| PSP | core 480x272, stream 848x480 | core 960x544 (`TOTV_STREAM_PSP_RES=1x\|2x\|3x`), stream **960x544** |
| PS2 | core 1x, stream 640x480 | core `2x` (`TOTV_STREAM_PS2_RES=1x\|2x\|4x`), stream **960x720** (the core reports 1280x896, aspect 4:3) |
| SNES | 640x480 | unchanged (software core, 256x224 upscaled) |
| x264 | `ultrafast`, ABR 4000 kbps, VBV 3 frames, 60 fps always, x264's own thread count, level 3.1 | `superfast` (`TOTV_STREAM_X264_PRESET`), **CRF 22** (`TOTV_STREAM_CRF`), VBV ceiling = `TOTV_STREAM_KBPS` or by pixels (9000 kbps at 960x720, 6800 at 960x544, 3-16 Mbps), **VBV buffer 6 frames**, 4 explicit slice threads (`TOTV_STREAM_X264_THREADS`), **the core's real fps** (50 PAL N64, 59.94 as 60000/1001), IDR once a second, deblocking on, still Baseline + `zerolatency`, no B-frames |
| SDP | `profile-level-id=42e01f` | `42e0` + the level of the picture: 3.2 (`20`) for 960x720 / 960x544 / 1280x720, 3.1 for 640x480, 4.2 for 1280x960; `level-asymmetry-allowed=1` stays. pion only matches the profile part of `profile-level-id`, so Chrome (headless, here) accepted `42e020` |

Variables: `TOTV_STREAM_QUALITY` (`high` default, `safe`), `TOTV_STREAM_{N64,PSP,PS2}_SIZE=WxH` (even, 160x120..1920x1088), `TOTV_STREAM_WIDTH`+`HEIGHT` (both set: every GPU core, SNES excluded), `TOTV_STREAM_KBPS` (now the VBV ceiling; 0/unset = by size), `TOTV_STREAM_HOSTMS_P95_X10` (budget in tenths of ms, default 120 = 12 ms, negative = off). Core options given in `TOTV_STREAM_{N64,PSP,PS2}_CORE_OPTS` still win over the profile.
`safe` is the old sizes, scales and 4 Mbps ceiling, but with the new x264 (CRF), so its picture is cheaper than the old ABR one (1.6 Mbps instead of 2.9 on the same N64 scene).

**Aspect.** The host (`emu/glhost.h`) now blits the picture with the display aspect the core reports (`geometry.aspect_ratio`, updated by SET_GEOMETRY; the frame's own w/h when it reports none) into the fixed output: within 3% it fills the frame, otherwise black bars (the output FBO is cleared when the rectangle changes). Measured on this PC: N64 reports 640x480 / 1.3333, PSP 960x540 / 1.7778 (fills 960x544, 0.7% off), **PS2 NTSC reports 1280x896 / 1.3333**: its 640x448 frame is *meant* to be shown at 4:3 (non-square pixels), so filling 4:3 was already right and there was no stretch to fix; what the aspect code gives is the safety for a game that reports 16:9 or a different aspect. Forced `TOTV_STREAM_N64_SIZE=1280x720` shows the N64 picture as a centered 960x720 with black bars, not stretched.

**Budget / safe mode.** The host now sends the p95 of the per-frame total (core + blit + readback + convert) of each 120-frame window (`hostMs.TotalP95`, the 12th float of message `T`, which was unused). If it stays above 12 ms for 3 windows in a row the session logs `host over budget ...` (at most once a minute) and reports `overBudget:true` and, for a `high` session, `suggestQuality:"safe"` in `GET /v1/sessions/{id}`; a window back under clears it. The component never changes a running session (the child's output size is fixed when it starts, and a mid-game restart would drop the game): the Totv can offer the user a restart with `"quality":"safe"`. It does not look at the GPU: the glTimer span (`hostMs.GPU`) includes the core's CPU time and was not a usable saturation signal here (1-3 ms on PS2, 19 ms on N64 in the first harness).

### Measurements (harness `perf/run2.mjs`: isolated component on UDP 18590 + socket in /tmp, fake Totv serving the ROM, headless Chrome as the WebRTC client with a real `RTCPeerConnection`; 12 s of warm-up dropped; sampled every second; RTX 3060 at its idle clock (SM 210 MHz in `nvidia-smi`), 12 threads)

Inputs: A/Start alternating (menus), then stick + A + X after `PLAYAT` s. N64 Banjo-Kazooie BR (PAL, 50 fps) 160 s: intros and cutscenes (a separate 75 s run with faster Start skipping reached the first scene, "gameplay" below); PSP Patapon 110 s: **never left the title menu** (Cross/Start mashed in a second run, no change); PS2 Crash of the Titans 120 s: reached the "Enter your name" screen, not gameplay. "before" = HEAD of `publicacao-limpa`, "after" = this branch (`superfast`, CRF 22).

| run | stream | fps decoded | drops (browser/server) | kbps avg / p95 / max | avg QP | x264 ms p50/p95/max | hostMs total, window avg p50/p95 | per-frame p95 of a window (p50 / max) | CPU emu / component (of one core, avg) | GPU util avg/max |
|---|---|---|---|---|---|---|---|---|---|---|
| N64 before | 640x480 | 50.0 | 0 / 0 | 2754 / 3284 / 3386 | 12.3 | 1.2 / 1.6 / 2.2 | 5.1 / 6.2 | (not reported) | 25 / 33 | 7 / 16 % |
| N64 after | 960x720 | 50.0 | 0 / 0 | 2403 / 3712 / 5112 | 15.1 | 2.0 / 3.2 / 5.5 | 6.7 / 7.4 | 9.7 / 11.2 | 32 / 41 | 10 / 19 % |
| N64 "gameplay" before | 640x480 | 50.0 | 0 / 0 | 2935 / 3320 / 3354 | 17.2 | 1.1 / 1.7 / 3.1 | 5.4 / 6.0 | - | 26 / 32 | 7 / 13 % |
| N64 "gameplay" after (veryfast) | 960x720 | 50.0 | 0 / 0 | 3030 / 4140 / 4382 | 20.9 | 3.2 / 6.0 / 11.2 | 6.2 / 6.8 | 9.2 / 10.0 | 30 / 55 | 9 / 15 % |
| N64 "gameplay" after (superfast = default) | 960x720 | 50.0 | 0 / 0 | 3532 / 4768 / 5271 | 20.0 | 2.1 / 3.6 / 6.6 | 6.5 / 7.3 | 9.6 / 10.1 | 31 / 41 | 10 / 16 % |
| N64 "gameplay" after (ultrafast) | 960x720 | 50.0 | 0 / 0 | 4450 / 6139 / 6893 | 20.7 | 1.8 / 2.8 / 4.7 | 6.8 / 7.4 | 9.7 / 10.3 | 32 / 35 | 10 / 16 % |
| N64 "gameplay" `quality:"safe"` | 640x480 | 50.0 | 0 / 0 | 1593 / 2097 / 2366 | 21.3 | 2.5 / 4.5 / 6.7 | 5.3 / 5.7 | 7.4 / 8.6 | 25 / 43 | 7 / 13 % |
| PSP before | 848x480 | 59.94 | 0 / 0 | 859 / 1058 / 2451 | 0.4 | 1.0 / 1.4 / 2.4 | 5.2 / 5.6 | - | 32 / 31 | 9 / 15 % |
| PSP after | 960x544 | 59.94 | 0 / 0 | 149 / 232 / 1075 | 4.2 | 1.2 / 1.8 / 2.4 | 6.0 / 6.6 | 7.4 / 8.1 | 37 / 28 | 9 / 15 % |
| PS2 before | 640x480 | 59.94 | 0 / 0 | 3397 / 3838 / 3967 | 15.3 | 0.7 / 1.0 / 1.4 | 7.4 / 9.7 | - | 68 / 24 | 11 / 14 % |
| PS2 after | 960x720 | 59.94 | 0 / 0 | 4300 / 5743 / 8477 | 14.7 | 1.4 / 2.0 / 2.5 | 8.5 / 10.7 | 12.4 / 15.7 | 64 / 32 | 24 / 32 % |

No frame was lost or dropped anywhere (`lost 0`, `nack 0`, `pli 0`, `freeze 0`; no single second ended more than 1.3 fps below the nominal rate). The PSP bitrate fell because CRF spends nothing on a static menu where ABR 4000 kept spending (QP 0.4 means it was over-spending); N64 fell for the same reason on the dark/low-detail scenes. Bursts are bounded by the VBV: the highest second was 8477 kbps (PS2) under the 9000 ceiling.
**The 12 ms budget tripped on PS2 at 960x720** (per-frame p95 12.2-15.7 ms in some windows; 5 log lines in 120 s, `overBudget` false at the end): the 2x render makes the synchronous readback wait on the GPU (readback 4.1 ms against 1.95 ms at 1x; core 2.6, convert 2.0) while the frame time (16.7 ms) and the 59.94 fps held with 0 drops. So on this machine PS2 at 2x is at the edge of the margin the budget asks for, N64 and PSP are comfortably inside (p95 about 10 and 8 ms). `TOTV_STREAM_PS2_RES=1x` or `TOTV_STREAM_READBACK=pbo` are the knobs to try there; neither was measured here.
**Preset.** Same N64 scene, same CRF: `ultrafast` 4.45 Mbps (x264 p95 2.8 ms), `superfast` 3.53 Mbps (3.6 ms), `veryfast` 3.03 Mbps (6.0 ms, max 11 ms on an IDR). `superfast` is the default: it saves 20% of the bits for ~1 ms of encode time; `veryfast` saves another 14% for another ~2 ms and the IDR spikes. These numbers include the encoder at 4 threads (it takes 0.2-0.5 of a core more than before).
**Sharpness** (PNGs in `scratchpad/perf/out-*-v3/` after, `out-*-v1/` before; crops in `cmp-*.png`): PS2 "Enter your name" letters and the blue glyph outlines are crisp and the background texture is smooth, against softer, blockier edges before (the 2x core plus the 960x720 stream; the upscale from 640x480 was the blur); PSP title logo and menu text lose the stair-steps of the 480x272 core upscaled to 848x480; N64 polygon edges (Banjo's guitar, the scenery in the 1280x720 test) are clean at the 640x480 core resolution (looked at in the 1280x720 test frame and the new N64 frames; a pixel-exact before/after crop of the same N64 instant was not obtained, the cutscene timing differs between runs). The headless Chrome decodes in software (1.5 ms/frame at 960x720, 1.2 at 640x480).

### Not measured (do not read more into these numbers)

- **Glass-to-glass latency** (the loopback `make latency` tests were not rerun; encode + frame-to-wire grew by 1-2 ms at 960x720 and the Chrome decode by ~0.3 ms, a screen and a TV scaler were not in the loop).
- **The Fire TV Stick** (AFTSS, ARMv7, ~900 MB RAM): 960x720 H.264 Baseline at level 3.2 and 50-60 fps was not decoded on it; the claim that it handles 720p/1080p@60 in hardware is from the device's specification, not measured. Its Wi-Fi under a 4-9 Mbps burst and the TV's own scaling of a 4:3 960x720 picture were not tried either. A real Chrome with hardware decoding was not used (headless Chrome here).
- Gameplay of PSP and PS2 (menus only: the Patapon title menu did not react to Cross/Start, PS2 stopped at the name entry) and any scene of N64 beyond the Banjo intro and the first scene; bitrate and host time under a heavy PS2/PSP game are unknown (the PS2 budget trip suggests the heavy case will be above the idle one).
- Sizes above 960x720 end to end (1280x960 / level 4.2 was only unit-tested for the SPS, never decoded; N64 at 1280x720 was run for the pillarbox check only), `TOTV_STREAM_N64_RES=3x`, PSP `3x`, PS2 `4x`, `READBACK=pbo` at the new sizes, two sessions at once, and the cgroup/`CPUQuota` limits with the higher CPU use.
- `make test`'s real-core tests (`N64=1`, `PSP=1`, `PS2=1`) were not rerun; the harness above is what exercised the real cores.

## Docker

`Dockerfile` (multi-stage, Debian 13 "trixie", bases pinned by digest) has three targets: `build` (Go + `retro-emu` in C, x264/opus/EGL headers; discarded), `corebuild`
(toolchain used only by `scripts/fetch-cores.sh`; discarded) and `runtime` (the default, the image you run). **The image contains no libretro core, no ROM, no BIOS/firmware
and nothing under a non-commercial licence**: cores come from a directory you mount at `/opt/cores`.

```
docker build --target runtime -t ghcr.io/victobriel/totv-streaming:local .             # 192 MB (docker image size), uid 1000, no shell needed at run time
scripts/fetch-cores.sh                                                   # ./cores: mupen64plus_next + ppsspp (+ cores/system/PPSSPP via scripts/psp-assets.sh); ~6 GB scratch, several minutes
scripts/fetch-cores.sh --i-accept-snes9x-noncommercial snes9x            # OPT-IN, prints the licence warning without the flag and refuses
docker compose -f docker-compose.example.yml up -d
```

`scripts/fetch-cores.sh` builds inside a throwaway Debian trixie container (the same libc/libstdc++/libGL as the runtime image: cores built on your host can fail to load there), clones
pinned refs (`MUPEN_REF`, `PPSSPP_REF`, `SNES9X_REF`), strips, and writes only to the output directory (`CORES_DIR`, default `./cores`). mupen64plus-next and PPSSPP are GPL-2.0(-or-later); snes9x is
**non-commercial**: it is not built unless you pass the flag, and it must never go into an image you distribute. PPSSPP needs no PSP firmware (HLE); its assets (1.8 MB) are installed by
`scripts/psp-assets.sh` (the fetch script calls it). `.dockerignore` keeps `cores/` and `*.so` out of the build context, so a core can not end up in the image by accident.

### Running as non-root, read-only, no capabilities: what the child needs from the container

The component and the child run as uid 1000. The child (`emu/host.c`, `sandbox_netns`) calls `unshare(CLONE_NEWUSER|CLONE_NEWNET)` and, for PSP, also `CLONE_NEWNS`; then writes its own
`uid_map`/`gid_map`, `mount("/", MS_REC|MS_PRIVATE)` and `mount(tmpfs, "/dev/shm", MS_NOSUID|MS_NODEV|MS_NOEXEC)`. None of this needs a capability in the container (a user namespace is created
without one, and the caps live only inside the child's namespace, where the child drops them), so `cap_drop: ALL` and no `cap_add` are enough. What blocks it is Docker's **default seccomp profile**, which allows
`unshare` and `mount` only with `CAP_SYS_ADMIN`. Measured here (Docker 29.9.3, kernel 7.3):

| Run (`--read-only --cap-drop ALL --security-opt no-new-privileges`, uid 1000) | Result |
|---|---|
| `retro-emu --selftest`, default seccomp | `seccomp:true landlock:true netns:false` (Landlock ABI 11 works under the default profile; only the netns fails) |
| `retro-emu --selftest`, `docker/seccomp-totv.json` | all `true`, `netns:true` |
| probe: `unshare(USER|NET|NS)` + id maps + private mount + tmpfs on `/dev/shm` + 300 MB file, default seccomp | `unshare: Operation not permitted` |
| same probe, `docker/seccomp-totv.json` | `ok` (also with `seccomp=unconfined`, for contrast) |
| component, `docker run`/`compose up` with the custom profile | `isolation={Seccomp:true Landlock:true Netns:true}`; `/proc/1/status`: `CapEff 0`, `NoNewPrivs 1`, `Seccomp 2`; `/v1/health` with the bearer: 200, without: 401 |

Options considered for the relaxation, narrowest first:

1. **`docker/seccomp-totv.json` (chosen)**: Docker's default profile (moby/profiles) plus three rules: `unshare` only with flags that are a subset of `NEWUSER|NEWNET|NEWNS`; `mount` only when
   the flags argument is exactly `MS_REC|MS_PRIVATE` (0x44000) or `MS_NOSUID|MS_NODEV|MS_NOEXEC` (0x0e). No bind mounts, remounts, moves, `setns`, `clone(NEWUSER)`, `pivot_root`.
   Regenerate/audit with `scripts/make-seccomp.sh` (a diff against Docker's default is the whole delta).
2. `--security-opt seccomp=unconfined`: works, but drops the whole syscall filter (bpf, keyctl, perf_event_open, kexec... are open again). Rejected.
3. `--cap-add SYS_ADMIN` (what the default profile asks for): hands a root-capable process a far larger surface than a userns. Rejected.

**Risk of the relaxation**: processes in the container (the component and, above all, the emulator child before its own filter is installed) can create user namespaces and mount tmpfs inside them. That
exposes the kernel code that is reachable only from a user namespace (netfilter/nf_tables, overlay, FUSE-like paths: the historic source of container-escape CVEs) to the process that runs the **core**. The
child's own seccomp filter (installed before any core code runs) denies `mount`, `setns`, `socket`, `bpf`... again, and Landlock + `NoNewPrivs` stay; but the window before that filter is the dynamic loader and the GPU driver
init (see "Isolation with a GPU"). If you cannot accept it, run without the profile: the component starts, reports `isolation.netns=false` and, as before, **refuses the sessions that require the namespaces (PSP)**
with `isolation_unavailable`; it never runs them unisolated. Hosts with AppArmor (docker-default denies `mount`) or SELinux need their own extra step (compose file comments): not tested, this PC has neither.

### Decisions of the container

| Setting | Why | Tested |
|---|---|---|
| uid/gid 1000, `USER 1000:1000`, no shell/HOME (`/nonexistent`) | non-root; matches the Totv's socket/secret ownership | yes |
| `read_only: true` | nothing is written to the image | yes (compose + docker run) |
| tmpfs `/run/totv-tmp` (ROM downloads, `size=2560m`) and `/run/totv-rw` (`TOTV_STREAM_RW_TMP`, PSP's writable dir, `size=512m`), both `uid=1000,mode=0700` | `size=` is the only quota: a hostile ROM can fill the RW directory. 2.5 GiB only matters for PSP ISOs (up to 2 GiB, in RAM); `mem_limit: 4g` accounts for them plus the PSP's private `/dev/shm` (up to 1 GiB per session) | mounted and used by the component; **filling/limit behaviour not tested** |
| `cap_drop: ALL`, no `cap_add`, `no-new-privileges` | the component needs none (UDP port 8422 is unprivileged) | yes (`CapEff 0`) |
| custom seccomp (above) | the child's namespaces | yes (selftest, probe, component) |
| `network_mode: host`, no `ports:` | WebRTC media on a fixed UDP port toward the client's `iceHost`; the control API is a Unix socket on a shared volume, not TCP | host network: yes for startup; **a real WebRTC session from a container: no** |
| `pids_limit 256`, `mem_limit 4g`, `cpus 4`, `init: true` | PPSSPP runs ~41 threads; limits are guesses from the measurements above | accepted by Docker; not load-tested |
| `ENTRYPOINT` = the component only; `retro-emu` at `/usr/local/bin` | single process tree | yes |
| `NVIDIA_DRIVER_CAPABILITIES=graphics,utility` | `graphics` makes the Toolkit inject libEGL_nvidia/libGLX_nvidia/libnvidia-eglcore...; `video` (NVENC) is not needed, encoding is x264 | **no (no Toolkit here)** |
| image ships `libegl1 libgl1 libx11-6` (glvnd dispatch only; Mesa and LLVM purged, -218 MB) and `/usr/share/glvnd/egl_vendor.d/10_nvidia.json` | `retro-emu` `dlopen`s `libEGL.so.1`; the cores link `libGL.so.1`/`libX11`; the Toolkit supplies the vendor libs and the ICD file | libs resolve (`ldd`); EGL on a real GPU: **no** |
| `--build-arg MESA=1` | Intel/AMD: keeps Mesa EGL/GL and `mesa-va-drivers` | **not built, not run** |

### What was really built and run (this PC: Docker 29.9.3, rootful, no NVIDIA Container Toolkit, no `nvidia-ctk`, no Podman)

- `docker buildx build --target runtime --load`: ok, 1 min 15 s warm, **192 MB**. A first attempt on Debian 12 failed: its kernel headers (6.1) lack the Landlock ABI 4/5 constants that `emu/host.c` uses, hence trixie. The `build` stage also checks that `libEGL.so.1` is present in `retro-emu`, i.e. that `HAVE_EGL` was compiled in.
- `ldd` of `libGL.so.1`, `libGLX.so.0`, `libEGL.so.1` and of the two binaries: nothing "not found".
- the selftest, namespace probe, component start (`docker run` and `docker compose up`, host network, seccomp profile resolved by Compose, tmpfs with `size=`, secret volume read-only, `/v1/health` over the socket): results in the table above.
- `scripts/fetch-cores.sh mupen64plus_next` (outside the repo, `CORES_DIR`): ok in about 2 minutes, a 4.3 MB `mupen64plus_next_libretro.so`; `ldd` inside the runtime image resolves everything (libGL/libGLX/libGLdispatch from the image). It was **not loaded or run**: no GPU in a container here. `scripts/fetch-cores.sh snes9x` without the flag: prints the licence notice and exits 3 before building anything.
- `scripts/fetch-cores.sh ppsspp` was started and **aborted on purpose**: it clones PPSSPP with submodules and `/home` fell to 3.3 GB free; the PPSSPP build, its assets step and the pinned tag `v1.20.4` (not the commit used in the measurements above, which was not recorded) are untested.

**Not proved**: any run with a GPU (the host has an RTX 3060 but no Container Toolkit, so `/v1/health` reported `gpu: null` and no EGL context was created in a container); a real session (WebRTC, a core, a ROM) from a container; the PSP private `/dev/shm` through the real emulator in a container (the
`unshare`+`mount`+tmpfs sequence was proved with a probe that copies `sandbox_netns`, not with `retro-emu` and PPSSPP); `fetch-cores.sh` for `ppsspp` (aborted, see above) and `snes9x` (flag check only); the Mesa/Intel/AMD variant; AppArmor/SELinux hosts; rootless Docker and Podman; behaviour when the RW tmpfs fills;
The glibc of the cores built by `fetch-cores.sh` is trixie's; cores already built on Arch (the ones used in the measurements above) were not tried in the image.

## What was proved (command -> result), on this PC (Linux 7.3, x86_64)

| Command | Result |
|---|---|
| `make test` | `ok totv-streaming` (also with `go test -race`) |
| `bin/retro-emu --selftest` | all keys `true` (`netns` where user namespaces exist): real blocked UDP and TCP `socket()`, `ptrace()`, `io_uring_setup`, `kill` of the parent, `open("/etc/passwd")`, and an x32 syscall killed by `SIGSYS`, from inside the sandbox |
| `TestCoreRunsOnlyInsideSandbox` | probe core: its constructor (at `dlopen`) can open `/etc/passwd`, its `retro_load_game` cannot, nor create a UDP socket nor `ptrace`: core code runs only after the sandbox |
| `TestParentChecksKernelView`, `TestCheckProcStatus` | the parent reads `Seccomp: 2` / `NoNewPrivs: 1` from `/proc` and rejects anything else |
| `TestSecondHelloAndSRAMLimits`, `TestMaxChildMsg` | a second hello does not panic nor change state; `S` only up to the announced `sram_size`; per-type size limits; at most one upload in flight |
| `TestAuth` | no/wrong secret gives 401 `unauthorized`; right secret 200; secret rotation without restart |
| `TestHealthContract` | `version, encoder, gpu, cores, isolation{seccomp,landlock,netns}` present with the right types; `netns` equals what the selftest proves |
| `TestSessionValidation` | `serverUrl` other than `http://127.0.0.1:*`, wrong platform/core/ids, path-traversal core: 400; ROM server answering 302: 502 (redirect not followed), nothing left behind |
| `TestSessionRefusedWithoutIsolation` | 503 `isolation_unavailable` |
| `TestSessionEndToEnd` | the route sequence the Totv uses: create 201, second create 409, list, signal offer gives answer, **a pion client connects over loopback** and receives H.264 and Opus RTP; **100% of the video packets checked carry playout-delay 0/0 with the negotiated extension id**; input over the unordered DataChannel is applied, an older `seq` and a malformed packet are dropped and counted; `DELETE` gives `{ok}` after the SRAM was `PUT` (counter 1); a new session loaded that save and saved counter 2 (A9 on the test core); the Totv saw no request without the token |
| `TestNoSignalTimeout` | session without signal is ended |
| `TestChildHasNoTokenAndNoEnv` | child has empty environment and no token in its arguments; sandbox flags include seccomp, landlock and landlock net (7, plus 8 with netns) |
| `TestInputPacketRoundTrip`, `TestSeqWraparound`, `TestButtonMapping` | 12-byte format, wrap-around, bit mapping |

## Real SNES (snes9x) proved on this PC

snes9x (`libretro/snes9x` at `204ea1c`, `make -C libretro`, 3.3 MB `.so`) was built outside the repo and copied to `cores/snes9x_libretro.so`
(ignored by git; internal test only, non-commercial licence, never distribute). ROMs are read from `<rom-dir>` and are only served by the
fake Totv from memory: no ROM is ever copied into the repository.

```
TMPDIR=<dir with space> make snes     # SNES=1 go test -run TestRealSNES  (env SNES_ROM, SNES_OUT, SNES_BOOT_SEC, SNES_TIMELINE)
```

| Check | Result |
|---|---|
| `<snes-rom>.zip` (zip, 1 `.sfc` 512 KiB) through `POST /v1/sessions` with `core: server_snes9x` | unpacked, loaded by the real core, sandbox flags 7, session `playing` |
| H.264 received by the pion test client, decoded with ffmpeg to RGB | Nintendo logo, then the title screen, colors correct (core uses RGB565); capture `snes-frame.png`: 1000+ distinct colors |
| Start (packet bit 14) sent over the DataChannel | screen goes from the title to the file select screen (an empty-slot list), mean abs pixel diff 19; capture `snes-frame-after-start.png` |
| a second SNES game zip | loads, title screen drawn, Start changes the screen (diff 15) |
| `TestPixelFormats` | the test core built as RGB565 and 0RGB1555 gives the same I420 as XRGB8888 (max diff 5 and 6 of 255) |
| `TestUnzipROM`, `TestNormalizeROM` | first `.sfc/.smc`, `../` and absolute names skipped, no ROM in zip, size over the limit, corrupt zip, nothing written besides the ROM |
| `make latency` (test core, after these changes) | press p50 46.8 ms, p95 48.0 ms; release p50 51.8 ms (same as before: no regression) |

Latency with snes9x was NOT measured (no ROM with a deterministic input-to-pixel effect and a known frame); the numbers below are with the test core.

## Latency measurable on this PC (`make latency`, loopback)

Pipeline: test core (60 fps) -> I420 640x480 -> x264 -> RTP -> pion client -> ffmpeg software decoder -> 16x12 grey. A "press" (RetroPad A, the
core paints white) is sent over the DataChannel and the time until the decoder outputs a white frame is taken, 30 presses, two runs:

| Measure | Result |
|---|---|
| Input to decoded frame, press | p50 46.6 and 47.7 ms; p95 50.4 and 50.6 ms |
| Input to decoded frame, release | p50 51.5 and 51.2 ms; p95 53.6 and 53.0 ms |
| Server stage: DataChannel packet to child pipe write | p50 0.006 ms |
| Server stage: pipe write to next emulated frame arriving at the parent | p50 7 to 13 ms (wait for the 60 fps frame boundary: it is the frame period, not overhead) |
| Server stage: x264 encode of one frame | p50 ~1 ms, p95 ~1.5 ms |
| Server stage: frame arrival to RTP written | p50 ~1.2 ms, p95 ~1.9 ms |

Also available at runtime in `GET /v1/sessions/:id` (`stats`). The rest of the ~47 ms (about 30 ms) is loopback + the client side
(pion, ffmpeg depacketize/decode/scale), which is NOT representative of the Fire TV decoder. These numbers say that the server stages
use about 15 to 20 ms of the 80 ms budget; they are NOT the glass-to-glass latency of criterion A2.

## What was NOT proved

- **PSP:** see "Not done / not proved (PSP)" above: no commercial game, so real-game load, compatibility, long runs and big ISOs are unmeasured.

- **A2 glass-to-glass latency** (240 fps camera, Fire TV, Wi-Fi): not measured. No Fire TV in this test, no display.
- **A1 start time, A3 fps/dropped frames in `getStats()`, A5 hardware decoder name, A7 30-minute stability / memory growth, A8 TV RAM**: not measured.
  The 60 s probe runs showed no dropped frames on the server (`framesDropped: 0`), nothing more.
- Interoperability with the real Android WebRTC client (Fire TV) and with browsers: only a pion client was used. The playout-delay
  extension was proved to be sent and negotiated with that client, not that libwebrtc on Android honors it (A4 on the TV).
- Real core beyond what is listed above: only one SNES game was driven with input. a second SNES game loaded and rendered its title screen and Start changed the
  screen; a third SNES game loaded and rendered (intro) but the Start check was not satisfied in the 14 s waited (not investigated, probably still in its intro). Save RAM of a real
  game, audio of the real core, and long runs were not checked. Sandbox flags were 7 (seccomp+landlock) with the real core.
- The snes9x build is the one built locally for tests (`cores/`, ignored by git, never distributed). No Docker image contains snes9x (never built in).
- Docker: image built and the component run in it (non-root, read-only, no caps, custom seccomp); GPU, real sessions and the cores' build for PSP/SNES in the container are not proved: see "Docker".
- Real Totv: only a fake Totv (httptest) was used. The save endpoints and the `Authorization: Bearer` header for the component token
  were taken from reading the Totv routes, not exercised against it; `server_*` core id handling is unverified there.
- Key frames: a lost-picture request (PLI/FIR) forces an IDR, otherwise one per second (`keyint` 60). Not tested with packet loss. No bitrate adaptation (congestion control) is implemented.
- Media is only restricted by the UDP socket bound to `iceHost`; traffic from other IPs is not rejected (the contract's "only to the client IP" is not enforced).
- 720p/8 Mbps, multiple players per session (the packet carries `port`, the child accepts 0..3, SNES multitap untested), audio quality (linear resampler), A/V sync.
- Security review: one review done; its HIGH/MEDIUM findings (sandbox after core code ran, child-reported flags trusted, x32/io_uring/kill gaps, unguarded hello and SRAM messages) are fixed above. Open: deny-list instead of allow-list (see Isolation); `netns` depends on the host allowing unprivileged user namespaces.

## Hardening (revision 2)

Findings of the second security review and what was done (branch `hardening`; tests in `isolation_test.go`, `hardening_test.go`, `savezip_test.go`, `savezip_corpus_test.go`).

| Finding | Fix |
|---|---|
| A1 (high): the sandboxed core still acted on other processes of the same uid | seccomp: `rt_sigqueueinfo` (arg0 = own pid), `prlimit64`, `sched_setaffinity`, `sched_setscheduler`, `sched_setattr`, `sched_setparam` (arg0 = 0 or own pid), `setpriority`, `ioprio_set` (`which` = process, `who` = 0 or own pid; `PRIO_PGRP`/`PRIO_USER` denied) are limited to the process itself, everything else EPERM. Landlock ABI >= 6 adds `scoped = SIGNAL \| ABSTRACT_UNIX_SOCKET` (hello flag 64). `--selftest` proves each denial against the parent (and that the same calls on itself still work), and the Landlock scope on its own in a forked child before seccomp |
| M6: namespaces from inside | `unshare` -> EPERM; `clone` with any `CLONE_NEW*` -> EPERM; `clone3` -> ENOSYS (glibc >= 2.34 falls back to `clone`). Verified with real PPSSPP (41 threads) and mupen64plus-next |
| M1: threads before the sandbox | the host reads `Threads:` of `/proc/self/status` right before `sandbox()` and refuses the session (`error` message, no hello) unless it is 1 (Landlock `restrict_self` applies to the calling thread only). The NVIDIA EGL context creation does **not** start threads here (RTX/driver of this machine: N64 and PSP sessions start normally), so the context is still made before the sandbox on the main thread. Test: `threadcore` (a constructor that starts a thread) |
| M2: per-session limits | free space: `statfs` of the TMP dir before a download writes a byte (`Content-Length` + 64 MiB, unknown length: the platform maximum), refusal `no_space`; the writable directory needs 64 MiB free. Quota: while the session runs the size of the writable directory is measured every 5 s (`TOTV_STREAM_RW_MAX_MB`, default 512) and the session is killed and **not saved** above it. `RLIMIT_FSIZE` in the child before the sandbox (`TOTV_STREAM_MAX_FILE_MB`, default 512; host option `fsize=`). The private `/dev/shm` of the PSP is `size=512m` (was 1g; PPSSPP needs ~390 MB and ran with it). `TOTV_STREAM_RW_TMP` is now **checked**: it must be a tmpfs (`statfs` `f_type`), otherwise PSP sessions are refused (`rw_tmp_unusable`; `TOTV_STREAM_RW_TMP_ALLOW_DISK=1` for development only), a warning is logged at start when it is a tmpfs of more than 8 GiB (mount it with `size=`), and it must **not** be under `/dev/shm` (the core's private `/dev/shm` hides it) |
| M4: save after a crash | the parent only packs and uploads the final save after a clean exit (the host's `X` message). After a crash or a kill, files modified less than 2 s ago mean "still being written": nothing is packed and the last periodic upload stays the save |
| A2: DELETE during start | `stop()` no longer waits for a child that was never started: `start()` observes `quit` (its context is cancelled, the download is abandoned, no child is launched), `exited` is also closed by `teardown`, and `cmd`/`dir`/`rwDir` are guarded by a mutex (`go test -race`). Test: a fake Totv that stalls the ISO download; DELETE returns in well under 3 s, no child, no `ses-*` directory and no session goroutine left |
| M3: one rule for the save package | **refused** (the whole package): a name that could leave the directory (`..`, `.`, empty segment, absolute, backslash, drive letter, NUL/control characters), a symlink in any position, ZIP64, sizes over the limits (4096 entries, 256 MiB per entry and unpacked, 64 MiB packed) or a lying header, a duplicate name or `a/` next to `a`, and, for an entry that would be extracted, encryption or an unknown method. **Ignored** (never extracted, never an error): directory entries and any name outside `PSP/SAVEDATA/`. Parity with the Totv's corpus (`testdata/save-zips`, 32 zips): after fixing `a/` + `a` all agree. The component is stricter than the Totv in: empty segments (`a//b`), at most 4096 entries, 256 MiB per entry; invalid UTF-8 in a name is accepted (extracted as raw bytes, never able to escape) |
| low: `zipSaveDir` | `os.OpenRoot`, directories opened with `O_NOFOLLOW\|O_DIRECTORY`, files with `O_NOFOLLOW\|O_NONBLOCK` and `fstat` must say "regular file" (a FIFO swapped in cannot block, a link cannot be followed) |
| low: orphans | at start `ses-<digits>` and `rw-<digits>` directories (real directories, owned by this user) left in `TOTV_STREAM_TMP` and `TOTV_STREAM_RW_TMP` by a killed component are removed. Do not share these directories between two components |
| low: `TOTV_STREAM_GPU_DEVICES` | entries with `..`, `//`, `/./` or any non-clean path are dropped |
| low: `close_sockets` | walks `/proc/self/fd` (the old loop stopped at fd 1023) |
| low: tests | no personal paths nor commercial ROM names: `SNES_ROM`, `N64_ROM`, `PSP_ROM` are required (the tests skip without them) |
| low: licenses | full MIT notice in `emu/libretro.h`, `scripts/psp-homebrew/LICENSE-PSPSDK` (BSD), `scripts/psp-assets-PPSSPP-LICENSE.txt` (PPSSPP's `LICENSE.TXT`: its assets are copied by `psp-assets.sh`) |

**Evaluated, not done: `CLONE_NEWPID`.** The caller of `unshare(CLONE_NEWPID)` does not enter the new namespace, only its children do, so the host would need one more `fork` and a waiter process. The parent's checks (`verifyChildSandbox` reads `/proc/<pid>/status` of the process it started, the kill on timeout) would then look at the waiter, not at the process that runs the core, and killing the waiter does not kill the core without extra `PDEATHSIG` work. With the seccomp self-only list, the Landlock signal scope and Landlock denying `/proc`, the remaining gain is small; not worth a new failure mode for N64/PSP/SNES.

Known limits left: threads of the core that call `sched_setaffinity`/`setscheduler`/`setpriority` with their own tid (not the pid) now get EPERM (PPSSPP and mupen ran fine; they keep default scheduling); the writable-directory quota is sampled every 5 s, so the hard bound in between is the tmpfs `size=` (and `RLIMIT_FSIZE` per file); the private `/dev/shm` is bounded by its mount (512m) and is not part of the quota; the Landlock scope needs ABI >= 6 (kernel 6.12+), older kernels keep the seccomp list only; Landlock and TSYNC still apply per thread, which is why the single-thread rule exists.

## Hardening (revisão 3)

Findings of the third security review (branch `hardening2`; tests in `hardening3_test.go`, `hardening_test.go`, `savezip_corpus_test.go`, `isolation_test.go`).

| Finding | Fix |
|---|---|
| A-1 (high): without Landlock's signal scope (kernel < 6.12) a core could `fcntl(0, F_SETOWN, parent)` + `F_SETSIG(SIGKILL)` + `O_ASYNC` on its stdin pipe, and the parent's next input write killed the parent (or any process of the same uid) | seccomp: `fcntl` with `F_SETOWN`(8), `F_SETSIG`(10), `F_SETOWN_EX`(15) and `ioctl` with `FIOSETOWN`(0x8901), `SIOCSPGRP`(0x8902) return EPERM (only the low 32 bits of the command are compared, like the kernel's `int cmd`). `F_GETFL`/`F_SETFL(O_NONBLOCK)`/`F_GETFD` keep working. A library **constructor** runs at `dlopen()`, before the filter, so right before the sandbox the host also clears `O_ASYNC`, the owner and the signal number on every open descriptor (`scrub_async`). `--selftest` proves it on a pipe of its own (each command EPERM, the ordinary ones fine, nobody owns the pipe) and `scope_ok` is no longer 1 by default: below ABI 6 it is true only when that seccomp proof held. Test: `attackcore` (`-DTC_ATTACK`) tries the attack in a constructor and inside the sandbox while a *victim* process (a copy of the test binary, playing the parent) writes input; the host runs with the undocumented test option `noscope=1` (no Landlock scope, i.e. the old-kernel situation). With the **previous** `host.c` plus that option the victim is killed in both variants (`signal: killed`); with this one it survives and the core's log shows `F_SETOWN=-1(1)`, `armed=0` |
| health | `GET /v1/health` -> `isolation` now carries `landlock_abi`, `landlock_scope` (Landlock signal scope active), `async_owner_blocked` (the seccomp protection above, the one that matters on kernels < 6.12), `ipcns`, `fork_blocked`, next to `seccomp`/`landlock`/`netns`. The component logs a warning at start when neither `landlock_scope` nor `async_owner_blocked` holds. The session still needs `seccomp` (which includes all these proofs) to start |
| M-a: SysV IPC / POSIX queues of the same uid | `unshare(CLONE_NEWIPC)` together with user+net(+mount): the segments, queues and semaphores of the host's IPC namespace do not exist for the core. `scripts/make-seccomp.sh` allows bit `0x08000000` in the `unshare` rule and `docker/seccomp-totv.json` was **regenerated** (network was available; base = moby/profiles at the pinned commit, the only difference from the old file is that mask and the comment). Selftest: it creates and attaches a SysV segment *before* the namespace, marks it for removal, and after the sandbox `shmat()` of that id fails with EINVAL (`sysv_unreachable`). Fallback: if the container's seccomp profile predates the flag, the host retries without `CLONE_NEWIPC` (`ipcns:false`, hello flag 128 absent, warning in the log) and then the child's own filter denies `shmget/shmat/shmctl/shmdt/msg*/sem*/mq_open` outright (`--selftest noipc` proves that path) |
| M-b: no ceiling on processes/memory without a cgroup | (1) **`RLIMIT_NPROC` was evaluated and not used**: the kernel compares it with the number of processes of the *real uid across the whole machine* (not of the child), so a value low enough to matter blocks every thread of PPSSPP (41) and of the desktop's other processes of that uid, and a value high enough is no ceiling. (2) seccomp now denies `fork`, `vfork` and `clone` **without `CLONE_THREAD`** (`clone3` was already ENOSYS, glibc falls back to `clone`): no new processes at all, only threads. Real PPSSPP (41 threads), mupen64plus-next and snes9x ran normally. The selftest proves it in a forked child that stacks the real filter (fork/clone/vfork EPERM, `pthread_create` works). (3) at start the component reads its own cgroup (`/proc/self/cgroup` + `/sys/fs/cgroup`, **every ancestor**, tightest value) and, when `pids.max` or `memory.max` is not set, logs a `WARNING` and reports it in `/v1/health` -> `limits` (`pids_max`, `memory_max`, `pids_limited`, `memory_limited`, `warnings`). **Requirement**: run the component in a cgroup with a thread limit and a memory limit: systemd `TasksMax=` and `MemoryMax=` in the unit, or `pids_limit`/`mem_limit` in compose (the example already has them; PPSSPP needs ~41 threads, PSP ~250 MB + a 512 MiB `/dev/shm` + ISO copy). Threads and memory are **not** bounded by anything else |
| low 9: inodes | the quota of the writable directory (`dirUsage`) now also counts entries (files, directories, links): `TOTV_STREAM_RW_MAX_FILES`, default 20000 (real PPSSPP: a few hundred); over it the session is ended and not saved, like the byte quota (test `FILLINO`: 300 empty files, limit 100). The compose example mounts the tmpfs with `nr_inodes=20000` (rw) and `nr_inodes=4096` (tmp), the real bound between two samples (5 s) |
| low 10: PDEATHSIG | children are started with `Pdeathsig=SIGKILL`, **from one goroutine that does `runtime.LockOSThread()` for the life of the component** (`startChild`). The signal fires when the *thread* that created the child exits; with a dedicated locked thread that only happens when the whole process dies (crash, SIGKILL, panic), and not when Go retires some worker thread in the middle of a session. Chosen over "the protocol pipe EOF" because a core stuck inside `retro_run` never reads stdin; the EOF path (`drain_input` returning -1) still exists for the normal case. Test: a copy of the test binary starts a child through `startChild`, is killed with SIGKILL, and the emulator process is gone within 3 s. Not done in `host.c` (`prctl` there would be the same thread-bound signal, set after the fork instead of atomically) |
| low 6: Docker `mount` rule (flags `0x0e`) accepts any file system type | **cannot be narrowed in seccomp**: the type is a pointer (arg 2) and the filter sees only the pointer value. Documented in `scripts/make-seccomp.sh`: the rule is only useful with `CAP_SYS_ADMIN` in the child's own user namespace; the child drops every capability right after its own two mounts and its filter denies `mount`/`umount2`/`unshare`/`setns` before any core code runs, so the only mounts that ever happen are the host's two |
| low 7: supply chain | `scripts/fetch-cores.sh`: refs are **full commit SHAs only** (PPSSPP `v1.20.4` -> `fa50bb1976065c4f8b1b47af227d367fe9771555`, the commit under the tag; mupen and snes9x were already SHAs) and `git rev-parse HEAD` must equal the pin after each checkout or the build stops (`pinned()`). `scripts/make-seccomp.sh` takes a moby/profiles commit (default `a0fa70805f742f7da15b367770d802ea2a86c68b`) and refuses a branch name. The script runs were **not** repeated (the PPSSPP/mupen builds take hours and gigabytes): only `sh -n`, the argument check (a tag is refused) and a real regeneration of the seccomp JSON |
| M-c: save zip parity | `testdata/save-zips` now has the Totv's new cases, same `manifest.json` format (`file`, `expect`, `maxUnpacked`, `why`): `bad-dot-segment`, `bad-empty-segment`, `bad-empty-zip`, `bad-file-and-parent`, `bad-long-name` (513 bytes), `bad-too-many-entries` (4097), plus `bad-entry-too-big`. The zips were copied byte for byte from the Totv's generator output (`server/test/fixtures/save-zips/`, read only; nothing written there). The component already refused all of them: `TestSaveZipCorpusParity` passes with them |

Tests: `make test` (-race) green; real, one at a time: `SNES=1` (`TestSNESKeepsStreaming`, `TestRealSNES`), `N64=1` (`TestRealN64`, `TestRealN64Save`, a commercial N64 game), `PSP=1 PSP_KIND=game` (`TestRealPSP`, the commercial PSP game) and `make gl`. Sandbox flags of the real sessions: 207 (seccomp, landlock, landlock net, netns, landlock scope, ipc).

Known limits left: the cgroup ceiling for threads and memory depends on the operator (warning + health only); the fork denial would break a core that spawns processes (the three tested ones do not; `system()`/`popen()` there would get EPERM); in the IPC fallback SysV IPC is denied rather than namespaced; a constructor that runs before the sandbox can still do anything the *host* process could do at that point (it is the known "core code before the sandbox" limit, see "Isolation"), only the descriptor signal state is scrubbed afterwards; the Docker image, the new seccomp profile inside a container, `fetch-cores.sh` itself and Intel/AMD GPUs were not run in this round; `noscope=1` exists only to test the old-kernel situation (a parent that is already trusted passes it, the HTTP API never does).

## GPU isolation (opção 2)

Goal: reduce what a compromised core (a hostile ROM exploiting mupen64plus/PPSSPP) can reach through the NVIDIA driver, as far as possible **without root**, and ship scripts and a unit for the
part that needs root. Machine: Linux 7.3, RTX 3060, driver **"NVIDIA UNIX Open Kernel Module" 620.70.06**. The *open* kernel module is what this PC already runs (`nvidia-smi`/`/proc/driver/nvidia/version`): its kernel side
is public source and auditable, which is the better position to be in, but the user-space RM (GSP firmware, `libnvidia-*`) is still closed and the ioctl surface is the same (see "What is still reachable").
Nodes: `/dev/nvidia0`, `/dev/nvidiactl`, `/dev/nvidia-modeset`, `/dev/nvidia-uvm`, `/dev/nvidia-uvm-tools`, `/dev/nvidia-caps/*`, `/dev/dri/{card1,renderD128}` (the nvidia nodes and renderD128 are mode 0666 here).

### 1. Minimal descriptor surface

Method: `RETRO_EMU_FDX` (debug hook in `emu/host.c`; compiled only with `-DDEBUG` since the hardening review, so the default `bin/retro-emu` does not have it, and it needs `RETRO_EMU_LOG`) closes chosen descriptors right before the sandbox; one experiment at a time (GPU shared with the desktop), the child's `/proc/<pid>/fd` read at the hello and at the end; "works" = the
host's warm-up passes, no `E` message, **non-black picture** (Y range of the I420 frame) and no abort. There was no `strace` on this machine, so a second method was added: an `LD_PRELOAD` shim that makes `open()` of a chosen node fail with `EACCES`
(which also answers "does the *initialisation* need it?"). Baseline = `nvidiactl` x4, `nvidia0` x12, `nvidia-modeset` x2, `renderD128` x2; **`nvidia-uvm`, `nvidia-uvm-tools` and `nvidia-caps/*` are never opened by the EGL/GL path**.

| Experiment (glcore, 5 s unless noted) | Result |
|---|---|
| close `nvidia-modeset` x2 | works |
| + close `renderD128` x2 | works |
| + close **all** `nvidia0` x12 | works; **N64 (a commercial N64 game) 60 s and PSP (the commercial PSP game) 60 s: render, non-black** |
| keep only the first `nvidiactl` of 4 | `core FBO incomplete` (the useful client is not the first) |
| close one `nvidiactl` at a time: fd 3, 7, 16 | works; **fd 12: the driver aborts (exit -6)** |
| close nvidiactl fd 3+7+16 (keep 12) | works (glcore, N64 20 s) |
| preload denies `open()` of `nvidia-modeset` / `nvidia-uvm*` / `dri/card1` / `nvidia-caps/*` (even all together) | **works: glcore, N64 15 s, PSP 40 s render** |
| preload denies `renderD128` / `nvidia0` / `nvidiactl` | `eglInitialize failed`: needed **during init** |

Decision (`close_gpu_fds()` in `emu/host.c`, then the warm-up is run **again** and the pixel is checked, so a context that stopped working is an error at start, never a silently black stream): when the driver is NVIDIA's, everything
under `/dev/nvidia*` except `/dev/nvidiactl`, and everything under `/dev/dri/`, is **reserved by `/dev/null`** after the context exists and before the sandbox: each such descriptor is `dup3()`-ed over with one `/dev/null` (`O_CLOEXEC`) instead of `close()`d, so the number is not reused by a later `open()` (ISO, save, shm) and a stale use of it by the driver gets `ENOTTY`/`ENODEV` instead of acting on an unrelated file. The parent's check (`forbiddenGPUFD`) therefore sees `/dev/null` at those numbers, which it accepts; `TestGPUDescriptorSurface` compares the count against the `gfd=keep` control. The parent runs the check for every GL core (`coreInfo.gl`) and **fails closed**: an unreadable `/proc/<pid>/fd`, or no flag 256 with the NVIDIA driver, is a violation. `gfd=keep` (host option, tests only) disables it; a Mesa driver is left alone (it needs its render node open).
Reported in the hello (flag 256) and checked by the parent (`gpuFDViolations`: a session whose emulator still holds `/dev/nvidia<N>`, `-modeset`, `-uvm*`, `-caps/*` or `/dev/dri/card*` is refused with `isolation_unavailable`).

**Final list kept, and why** (child of a real GPU session, `/proc/<pid>/fd`): `/dev/nvidiactl` x4 (the RM client of the context: closing the one that matters aborts the driver; the other three are harmless to keep, see below), two pipes (control stdin, protocol) and `/dev/null` x2;
PSP also holds the ROM (read-only) and the deleted private `/dev/shm/ppsspp_0.ram`. **Not kept**: `nvidia0` (x12), `nvidia-modeset`, `renderD128`, uvm, caps, `dri/card*`.
Why not shrink `nvidiactl` further: its descriptor numbers are the order in which the driver opened them (not stable across driver versions), and the Landlock rule that exists since the first GPU version (`/dev/nvidiactl` `READ|WRITE|IOCTL_DEV`, needed because `libnvidia-eglcore` reopens it lazily) already lets the
core open any number of new RM clients, so fewer *inherited* descriptors would remove no capability.
Landlock's `IOCTL_DEV` only applies to descriptors opened **after** `restrict_self`: the inherited ones keep every ioctl (that is why the descriptors are closed instead of filtered).
Proof that the closed ones do not exist in the child: `TestGPUDescriptorSurface` (control run with `gfd=keep` holds `nvidia0` x12 + `modeset` x2 + `renderD128` x2; default run holds none), `TestGLCoreEndToEnd` (flags, fds of the real session), the same check on every GPU session at start.

Consequence for the **unit** (item 4): of the five nodes a naive allow-list would name, `nvidia-modeset`, `nvidia-uvm`, `nvidia-uvm-tools`, `nvidia-caps/*` and `dri/card1` can be **denied for the whole service** (measured above): `DeviceAllow=` lists only `nvidiactl`, `nvidia0` (init), `dri/renderD128` (init).

### 2. A cgroup per session (no root)

`cgroup.go`: each emulator runs in its **own transient scope** of the user manager, `systemd-run --user --scope -p TasksMax=256 -p MemoryMax=3072M -p MemorySwapMax=0 -p OOMPolicy=kill` (the scope is a child of `user@<uid>.service`, which systemd delegates to the user:
cpu, memory and pids; nothing outside the user's own subtree is created or touched). `systemd-run --scope` does not fork: it moves itself into the scope and `exec`s, so the pid the component started is still the emulator (or its supervisor), and
`env -i` clears what systemd-run needed (`XDG_RUNTIME_DIR`, bus address) so the emulator still gets an **empty environment** (test: `/proc/<pid>/environ` is empty). The component probes this once at start (a throw-away scope, limits read back from inside it) and **checks every session
against the kernel**: after the hello it reads `pids.max`, `memory.max`, `memory.swap.max` of the emulator's cgroup and compares.
`TOTV_STREAM_CGROUP` = `auto` (default: use it if the probe works, otherwise warn in the log and in `/v1/health`; once the start probe says `limits.session.state == "enforced"`, a session whose cgroup limits read back from the kernel do not match is **refused**, as in `require`: `auto` degrades only at the start probe), `off`, `require` (no session starts without an enforced cgroup); `TOTV_STREAM_SESSION_PIDS` (256; PPSSPP runs 41 threads), `TOTV_STREAM_SESSION_MEM_MB` (3072; N64 322-343 MB, PSP 220-241 MB RSS; the PSP tmpfs counts).
`/v1/health` `limits.session` = `{"mode","state":"enforced"|"not enforced","method","pids_max","memory_max_mb","swap_max_zero","sessions_verified","sessions_unverified","warnings":[]}` (the old `limits` fields about the component's own cgroup are unchanged; their "no memory.max" warnings are dropped when the session cgroup is enforced, because the component's cgroup is no
longer what bounds a core). The probe has a 15 s timeout, and the wrapper runs `env -i --` found with `exec.LookPath`. Fallbacks: no `systemd-run` / no user manager (component run as a system service) = **not enforced**, with the warning telling you to use the unit (`TasksMax=`, `MemoryMax=`) or `scripts/run-sandboxed.sh` (the whole component in one scope).
Not done: a second backend that creates sub-cgroups itself under a `Delegate=yes` unit cgroup (clone3 `CLONE_INTO_CGROUP`); under the unit of item 4 the unit's own limits bound the component and its (default: one) session.

Tests (real kernel, small limits, the GL test core compiled as hostile: `glhogt` = thread bomb, `glhogm` = memory hog, 2000 threads / 1 GiB hard caps in the code): `TestCgroupThreadBomb` (`pids.max=64`: **`pids.current` stops at 64, `pids.events max` > 0, health 200, a new session starts**),
`TestCgroupMemoryHog` (`memory.max=400M`: **`memory.events oom_kill 3 oom_group_kill 1`, the hog is gone, the component and the next session are fine**), `TestSessionCgroupEnforced` (limits read back, own scope under `/user.slice/user-1000.slice/user@1000.service/`, empty environ, health),
`TestCheckSessionCgroupReadsBack`, `TestWrapCgroupClearsTheEnvironment`, `TestCgroupRequireRefusesWhenUnavailable`, `TestCgroupModeOffReportsNotEnforced`. `TEST_CGROUP=require` makes every session of any test (the real cores below too) go through the scope.

### 3. PID namespace

`unshare(CLONE_NEWPID)` is requested together with the user/net/ipc namespaces (it needs the caps of the new user namespace, so before they are dropped); only *children* enter it, so `enter_pidns()` forks once right after: the **child is pid 1 of the namespace and is the emulator** (everything after runs in it), the process the component started
stays outside as a **supervisor** (it closes every descriptor but 0/1/2 and a lifeline pipe, sets `no_new_privs`, applies the same seccomp filter, and only `waitpid`s, mirroring the exit status or signal). Chain: component -(Pdeathsig, as before)-> supervisor -(`PR_SET_PDEATHSIG`)-> emulator; a lifeline pipe
closes the race "supervisor killed before the emulator set Pdeathsig"; when pid 1 dies the kernel kills everything left in the namespace. What it adds: the core (and any thread/process it manages to create) can name no process outside the namespace (kill/ptrace/pidfd by number, `/proc/<n>` of the host are not
addressable from inside), in addition to seccomp and Landlock scoping. **/proc is not remounted**: the namespace does not hide the host's `/proc` from the emulator, **Landlock does** (no rule grants `/proc`, so `/proc/<n>` of the host cannot be opened even though the files exist); the host's `/proc` keeps working for the parent's checks.
**Side effect of being pid 1:** the kernel does not deliver to init a signal whose handler is the default unless it comes from an ancestor namespace, so inside the namespace `SIGABRT`, `SIGPIPE` and `SIGXFSZ` (and other default-action signals) that the emulator raises on itself are ignored instead of killing it. Consequences (reasoned from the kernel rule, **not measured here**): a self-raised `SIGABRT`/`SIGPIPE` does not end the emulator with a signal status, so the exit codes the supervisor mirrors for those cases differ from the pre-namespace ones (do not match tests or logs on "killed by SIGABRT"); and for `RLIMIT_FSIZE` (`TOTV_STREAM_MAX_FILE_MB`) the oversize `write()` fails with `EFBIG` while `SIGXFSZ` is ignored, instead of the process being killed. The limit still holds (the bytes are not written); only how it shows up differs. SIGKILL/SIGSTOP from outside the namespace still work, which is what the supervisor and the timeouts use.
Parent side (`isolation.go`, `session.go`): `corePID()` finds the emulator as the supervisor's only child whose `NSpid` ends in 1 (`/proc/<pid>/task/<pid>/children`); `verifyChildSandbox` (Seccomp 2, NoNewPrivs 1, every thread) runs on **both**; the netns, the descriptor check and the cgroup check use the emulator; hello flag 512 must agree with
`/proc/<emulator>/ns/pid` differing from ours; a kill by timeout hits the supervisor and the emulator follows through Pdeathsig (`TestPIDNamespaceChain` kills the supervisor and sees the emulator die). The tests that read `/proc/<pid>` of "the child" use `ss.corePID()`.
`retro-emu --selftest` has a new key `pidns` (run through the real `sandbox_netns`+`enter_pidns` in a forked process: pid 1, no visible parent). Fallback: a kernel or seccomp profile that refuses `CLONE_NEWPID` gets the sandbox without it (the same way as IPC), `isolation.pidns` in health is false and a session is still accepted; with `isolation.pidns` true a session whose child did not enter it is refused.
`TOTV_STREAM_PIDNS` = `auto` (default: a session runs without the namespace, with a warning at start, when the kernel/profile refuses `CLONE_NEWPID`) or `require` (a session is refused, even with `TOTV_STREAM_ALLOW_NO_ISOLATION=1`, unless the emulator is in its own PID namespace as seen in `/proc`). **Production and GPU deployments should use `require`** (the unit in `contrib/` does not set it, because the example assumes nothing about the seccomp profile; add `Environment=TOTV_STREAM_PIDNS=require` after checking `/v1/health` `isolation.pidns` is true).
Flags in the hello now: 1 seccomp, 2 landlock, 4 landlock net, 8 netns, 16 GPU context, 32 private `/dev/shm`, 64 Landlock scope, 128 IPC namespace, **256 GPU descriptors closed**, **512 PID namespace**.
Docker: the seccomp profile (`docker/seccomp-totv.json`) must allow `CLONE_NEWPID` in `unshare()`; NOT regenerated or tested here (no `make-seccomp.sh` run): without it the host falls back as above.

### 4. Dedicated uid (needs root; NOT executed here)

`scripts/setup-uid.sh --totv-user <login> [--dry-run] [--gen-secret]` creates the system user/group `totv-stream` (no shell, no home), adds it to `video`/`render`, adds the Totv's user to group `totv-stream`, creates `/etc/totv-stream/retro-stream.secret` (owner = the Totv's user, group `totv-stream`, 0640)
and the two empty mount points of the sized tmpfs; it never touches device modes/udev. `contrib/totv-streaming.service` is the matching unit. Checked **without installing anything**: `systemd-analyze verify` (clean, with `ExecStart=` pointed at an existing binary in a copy) and
`systemd-analyze security --offline=true`: **overall exposure 2.5 OK**. The remaining findings are `PrivateNetwork=` (the component serves UDP media: needs the host network), `PrivateUsers=` (the supplementary groups for the GPU nodes would be unmapped), `DeviceAllow=` (the three nodes, on purpose), `IPAddressDeny=` (site specific, commented) and `UMask=`.
The exec-environment subset that a **user** manager can apply (NoNewPrivileges, empty CapabilityBoundingSet, ProtectSystem=strict, ProtectKernel*, ProtectControlGroups, PrivateTmp, RestrictNamespaces, RestrictAddressFamilies, RestrictRealtime, RestrictSUIDSGID, LockPersonality, SystemCallFilter, SystemCallArchitectures) was **run for real** with
`systemd-run --user`: `TestPIDNamespaceChain`, `TestSelftest*`, `TestPSPFullPathSessionAndSave`, `TestGPUDescriptorSurface` and `TestGLCoreEndToEnd` pass under it. Findings from that run that shaped the unit: `AF_NETLINK` must be allowed (without it `failed to create network: route ip+net: netlinkrib: address family not supported by protocol`: Go's
`net.Interfaces()`, used by pion); `RestrictNamespaces` must keep `user net ipc pid mnt` (the child builds those); `@mount` and `@sandbox` (Landlock/seccomp syscalls) must be added to `@system-service`; `MemoryDenyWriteExecute` must **not** be set (N64 dynarec, PSP JIT, driver mappings). Not testable by a user manager: `User=`, `SupplementaryGroups=`, `DevicePolicy`/`DeviceAllow` (needs the system manager), `ProtectProc=` (hidepid was not applied), `TemporaryFileSystem=` of paths outside the user's reach. Those were reasoned, not run.

| Directive | What it gains | What it does not do |
|---|---|---|
| `User=totv-stream` `Group=` | the component, the emulators, their files and signals are a different uid from the Totv, the browser, ssh keys, the desktop session: a compromised core in userland cannot read or kill them (ptrace/kill by uid) | kernel exploit = root; nothing stops the core from attacking *other* things of `totv-stream` (only the component) |
| `SupplementaryGroups=video render` | access to the nodes on systems that restrict them | here the nodes are 0666: `DeviceAllow` is the real gate |
| `DevicePolicy=closed` + `DeviceAllow=` of `nvidiactl`, `nvidia0`, `dri/renderD128` rw | **cgroup device controller**: `nvidia-modeset`, `nvidia-uvm*`, `nvidia-caps`, `dri/card1` (the display's DRM master), disks, input, tty cannot even be opened by the service or by anything the core manages to run, even if Landlock and seccomp fell | `nvidiactl`/`nvidia0` stay: the RM ioctl surface is the exposure |
| `NoNewPrivileges` `CapabilityBoundingSet=` `AmbientCapabilities=` | no setuid/file-cap escalation inside the service; the host's `unshare(CLONE_NEWUSER)` still works (unprivileged) | not a kernel bug |
| `ProtectSystem=strict` `ProtectHome=yes` `ReadOnlyPaths=` `ProtectKernel*` `ProtectControlGroups` `ProtectClock` `ProtectHostname` | the service cannot write anywhere but its runtime dir and tmpfs; cannot touch `/sys`, `/proc/sys`, cgroups, the clock | read of world-readable files remains (Landlock already limits the core) |
| `ProtectProc=invisible` `ProcSubset=pid` | other uids' processes are invisible, non-process `/proc` hidden | the component still reads its own children's `/proc` (needed) |
| `PrivateTmp` + `TemporaryFileSystem=` x2 with `size=`, `nr_inodes=`, `noexec` | a ROM that fills `/tmp` or the PSP memory stick is bounded in RAM, not on disk | RAM is the cgroup's problem (`MemoryMax`) |
| `MemoryMax=4G` `MemorySwapMax=0` `TasksMax=512` `CPUQuota=400%` | the unit's cgroup bounds the component and all sessions (default `MaxSessions=1`) | no per-session limit (see item 2) |
| `RestrictNamespaces=user net ipc pid mnt` | only the namespaces the host builds for itself | the child could still create them *if* seccomp were gone (it denies `unshare`) |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_NETLINK` | no raw/packet/bluetooth/vsock sockets; IPv6 not needed (UDP4 media) | not an address filter: set `IPAddressDeny=any` + `IPAddressAllow=` for the Totv's LAN |
| `SystemCallFilter=@system-service @mount @sandbox unshare` | a second seccomp layer (reboot, kexec, modules, bpf, keyring... denied) under the child's own filter | allows what the host needs; not an ioctl filter |
| `RestrictRealtime` `RestrictSUIDSGID` `LockPersonality` `UMask=0027` | smaller surface; sockets/files not world-visible | |
| *not set*: `MemoryDenyWriteExecute`, `PrivateDevices`, `PrivateNetwork`, `PrivateUsers` | JIT (N64/PSP), the GPU nodes, UDP media and the video groups need them off | |

**Control socket and secret with the dedicated uid.** `RuntimeDirectory=totv-stream` (0750, owner `totv-stream`) holds `control.sock` (created 0660 `totv-stream:totv-stream` by `listenUnix`): the **Totv's user must be in group `totv-stream`** (`setup-uid.sh` does it; a new login is needed) and then talks to the component exactly as today (`--unix-socket /run/totv-stream/control.sock`, bearer token).
The secret stays a file the **Totv writes** (it is the owner, and can rotate it in place: the component re-reads it on every call) that the component reads through the group (0640): the component never needs to write it. Group membership is the authorisation boundary of the socket: anyone in `totv-stream` (and the Totv's uid) can start sessions. The dedicated uid protects the Totv *from the core*, not the component *from the Totv's uid*.
**What it does not solve**: a kernel exploit through the GPU driver is root regardless of uid (item 6); the driver and the 3 nodes are reachable by the uid; a second concurrent session shares the uid (and, with `MaxSessions` > 1, the unit's cgroup).

### 5. Policy for the Totv: `streamGpuPlatforms` (proposal, to be implemented by `servidor`; nothing changed in the Totv here)

N64, PSP and PS2 (everything that needs the GPU) should be a resource the admin **turns on knowingly**, because they put the closed NVIDIA ioctl surface within reach of a ROM. SNES (software rendering, no GPU node at all) stays on by default. Proposal: a server setting `streamGpuPlatforms`, default `[]`
(opt-in), a subset of `["n64","psp","ps2"]`; the server does not offer, and does not create sessions for, a GPU platform that is not listed, even if the component reports its core as available (`/v1/health` `cores`); the admin screen shows the warning below the first time any platform is switched on and on every change; the component's `gpu` and `limits.session` (state, `enforced`/`not enforced`) and
`isolation.pidns` are shown next to the switches, and a GPU platform with `limits.session.state` = `not enforced` shows a second warning. Suggested text (en-US key `games.gpuPlatforms.warning`):
"Nintendo 64, PSP and PS2 games run on this server's graphics card. A game that exploits its emulator can reach the graphics driver, which is closed-source code in the kernel, and could freeze the screen or compromise this machine. Only turn this on for ROMs you trust and on a machine where that risk is acceptable. Super Nintendo does not use the graphics card."
(pt-BR: "Jogos de Nintendo 64, PSP e PS2 rodam na placa de vídeo deste servidor. Um jogo que explore o emulador pode alcançar o driver da placa, que é código fechado dentro do kernel, e travar a tela ou comprometer esta máquina. Ative apenas para ROMs em que você confia e em uma máquina onde esse risco seja aceitável. Super Nintendo não usa a placa de vídeo.")

### 6. What a compromised core can still reach after all of this (honest list)

| Still reachable | Why | Mitigation | Residual |
|---|---|---|---|
| **RM ioctls through `/dev/nvidiactl`** (inherited x4 and any number of new ones, Landlock allows `IOCTL_DEV` on it) and the `nvidia0`-bound objects the context already holds | the GL path itself lives on them; the surface is the kernel RM interface of the open module (audit-able source) plus the GSP firmware | keep the driver current; close everything else (done); unit `DeviceAllow` (root); no per-ioctl allow-list was built (it would have to be re-recorded per driver version) | a bug in that interface is a **kernel** bug: root, whatever uid/namespace/cgroup |
| **VRAM without limit** | NVIDIA has no per-process VRAM cgroup (the `dmem` controller needs a driver that registers regions; not this one); MIG is data-centre only | `MaxSessions=1`, session watchdog (5 s "no video" ends the session), monitor `nvidia-smi --query-compute-apps=pid,used_memory --format=csv` | other GPU users (the desktop, browsers) can be starved of VRAM |
| **GPU hang / channel reset** (infinite shader, bad push buffer) | one shared GPU, one engine queue; a hang is an `Xid` event and the desktop compositor stalls with it | the watchdog ends the session, but a hung channel can outlive it; recovery without root is not possible (`nvidia-smi --gpu-reset` needs root and no clients; no PCIe reset); a second GPU or an iGPU for the stream; for N64 the software RDP (angrylion) was not measured | the desktop can freeze until the channel is reset or the user logs out/reboots |
| the driver's **initialisation code**, which runs before the sandbox | it has to (descriptors and mappings must exist first) | trusted, fixed inputs from the host | a bug there is outside the sandbox |
| generic kernel surface: `CLONE_NEWUSER` (and the namespaces under it) is created by the host itself | needed for the netns, the PSP `/dev/shm` and now the PID namespace | seccomp denies `unshare`/`setns`/new namespaces to the core afterwards | a kernel bug in namespace code is reachable only before the sandbox, by the host, with fixed arguments |
| everything the **uid** can do (without the dedicated uid: everything the Totv's user can do) | an unprivileged user can still read world-readable files, run as that uid | Landlock (reads limited to the library dirs), dedicated uid (item 4) | a kernel exploit ignores it |

**When option 3 (a microVM with its own GPU: virtio-gpu/Venus, or passthrough of a second GPU) becomes necessary:** (a) anyone other than the admin can bring ROMs (shared or multi-tenant use); (b) the machine holds data or sessions worth more than the GPU path (a shared server, not a personal PC); (c) a GPU/driver compromise = kernel compromise is not acceptable
for the deployment; (d) the driver cannot be kept patched promptly. A personal PC where the admin picks the ROMs, with option 2 plus the opt-in of item 5, is the intended use of the GPU platforms; for everyone else SNES (no GPU node) is the safe default and N64/PSP wait for option 3.

### Commands -> results (this PC, TMPDIR `<tmpdir>`, GPU shared with the desktop: one GPU test at a time)

| Command | Result |
|---|---|
| `make test` (go vet + `go test -race`) | `ok` 30.2 s |
| `make gl`, then `TEST_CGROUP=require make gl` | `TestGLCoreEndToEnd`, `TestHostOptionsSanitized`, `TestN64ButtonMapping`, `TestLatencyProbeGL` pass (also inside the session scope) |
| `TEST_CGROUP=require SNES=1 SNES_ROM=<your-snes-rom.zip> go test -run TestRealSNES` | pass, 16.6 s |
| `TEST_CGROUP=require N64=1 N64_SECS=60 N64_ROM=<your-n64-rom> go test -run 'TestRealN64$'` | pass: 60.01 fps over 68.5 s, RSS 322..343 MB, flags 991 (GPU fds closed, PID ns), 1 thread |
| `TEST_CGROUP=require PSP=1 PSP_KIND=game PSP_ROM=<your-psp-iso> PSP_DRIVE_BUTTONS=up,cross PSP_DRIVE_START=18 PSP_DRIVE_PRESS_SECS=4 PSP_SECS=60 go test -run 'TestRealPSP$'` | pass: 59.95 fps over 66.9 s, 229 distinct frames, RSS 220..241 MB, 41 threads (limit 256), fds = `nvidiactl` x4 + pipes + ISO + shm, flags 1023 |
| `systemd-analyze verify` / `security --offline=true` on the unit | clean / **2.5 OK** |
| the unit's exec subset under `systemd-run --user` (see item 4) | the sandbox, PSP, PID-namespace and GL tests pass; `AF_NETLINK` is required |

### Not done, and why

- **The unit and `setup-uid.sh` were not installed or run** (root): `User=`, groups, `DeviceAllow`/`DevicePolicy`, `ProtectProc`, the `TemporaryFileSystem=` mounts and the group-based socket access are reasoned and partially exercised, not run as that uid.
- A sub-cgroup backend under a `Delegate=yes` unit (per-session limits without a user manager).
- `nvidiactl` is not reduced below its 4 inherited descriptors (it would change nothing, see item 1); no ioctl allow-list; no VRAM limit exists to set; a GPU hang cannot be recovered without root.
- Docker seccomp profile not regenerated for `CLONE_NEWPID`; Mesa (Intel/AMD) left alone by the descriptor closing and untested.
- The `streamGpuPlatforms` policy is a proposal for the `servidor` area; the Totv was not changed.

