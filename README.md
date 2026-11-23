# totv-streaming

Game streaming component for the Totv media server (a separate, currently private project; this repository stands on its own and
documents the contract it speaks in [docs/CONTRACT.md](docs/CONTRACT.md)). It runs **one libretro core per
session** (SNES, N64, PSP or, as a prototype, PS2), encodes the picture as H.264 Baseline (no B-frames) and the sound as Opus, sends both over
WebRTC, and receives the controller over an unordered DataChannel.

This project is **free of charge and maintained by its author** (GPL-3.0-or-later, see [LICENSE](LICENSE)). It is a
**prototype**, not a product. Note on "non-commercial": the only non-commercial restriction here belongs to the **snes9x
core** (see [NOTICE.md](NOTICE.md)); it applies to that core, not to this project's own code. No new license is
invented here, and legal advice is still pending (see [ROADMAP.md](ROADMAP.md)).

## How it connects to the Totv

The Totv server talks to this component over a Unix socket (control API, `Authorization: Bearer <secret>`), hands it a
session token, and the component downloads the ROM and the save from the Totv on `127.0.0.1` and uploads the save back.
The contract (routes, authentication, input packet, saves, limits) is specified in
[docs/CONTRACT.md](docs/CONTRACT.md). The environment
variables `TOTV_STREAM_*` and the socket/secret paths are part of that contract and must not be renamed.

Nothing from this repository goes into the Totv image: it is a separate program (GPL) that the Totv only talks to.

## Requirements

- Linux x86-64, Go >= 1.24, gcc, make, `pkg-config` with `x264` and `opus` development packages.
- Optional, for the GPU path (N64/PSP): EGL/GL headers (`libegl-dev`) at build time and a GPU driver with headless EGL
  at run time. Tests of the GPU path need a real GPU.
- For the sandbox: a kernel with seccomp, Landlock (ABI >= 4 recommended) and unprivileged user namespaces. Without
  them the component refuses to start sessions unless `TOTV_STREAM_ALLOW_NO_ISOLATION=1` (development only).
- ffmpeg only for the latency probe test.

## Build

```
make build     # bin/retro-emu (C libretro host), test cores, bin/totv-streaming
make test      # go vet + go test -race (no GPU, no ROM needed)
```

## Cores (not included, never committed)

No core, ROM, BIOS or firmware is in this repository or in its Docker image. Build the cores yourself, inside a throwaway
container, at pinned commits:

```
scripts/fetch-cores.sh                                   # mupen64plus-next (N64) + PPSSPP (PSP), into ./cores
scripts/fetch-cores.sh pcsx2                             # PS2 prototype (PCSX2 libretro core, GPL-3.0); needs a BIOS of YOUR console, see "PS2"
scripts/fetch-cores.sh --i-accept-snes9x-noncommercial snes9x   # opt-in: the snes9x license is NON-COMMERCIAL
```

Read [NOTICE.md](NOTICE.md) for each core's license before you distribute anything you build. Note: the emulator host
(`emu/`, GPL-3.0-or-later) loads the core with `dlopen` **into the same process**. A GPL-3.0-or-later host and a
GPL-2.0-only core would be incompatible for *joint distribution* (shipping both together). Running them locally distributes
nothing. Which GPL-2.0 core versions are "or later" is still to be confirmed; see NOTICE.md and ROADMAP.md.

## Run

Development, with the built-in test core (any file works as a "ROM"):

```
export TOTV_STREAM_SOCK=/tmp/rs/control.sock TOTV_STREAM_SECRET_FILE=/tmp/rs/secret \
       TOTV_STREAM_CORES=$PWD/bin TOTV_STREAM_EMU=$PWD/bin/retro-emu TOTV_STREAM_DEV_TESTCORE=1
mkdir -p /tmp/rs && echo 'a-secret' > /tmp/rs/secret && chmod 600 /tmp/rs/secret
bin/totv-streaming
curl --unix-socket /tmp/rs/control.sock -H 'Authorization: Bearer a-secret' http://x/v1/health
```

Deployment examples (not installed by anything here): [Dockerfile](Dockerfile),
[docker-compose.example.yml](docker-compose.example.yml) (the image name is an example; **no image is published yet**),
[contrib/totv-streaming.service](contrib/totv-streaming.service) with [scripts/setup-uid.sh](scripts/setup-uid.sh) for a
dedicated unprivileged uid. All variables and the full API are described in [docs/DEVLOG.md](docs/DEVLOG.md).

Real-core tests read ROMs from your own disk and are skipped without them (`SNES=1 SNES_ROM=...`, `N64=1 N64_ROM=...`,
`PSP=1 PSP_ROM=...`; see [CONTRIBUTING.md](CONTRIBUTING.md)). ROMs are never part of the repository.

## Picture quality

Default profile `high` (`TOTV_STREAM_QUALITY=safe` or `"quality":"safe"` in a session's create request give the previous one): N64 renders at 640x480 and
streams 960x720, PSP renders 960x544 and streams 960x544, PS2 renders at 2x and streams 960x720; x264 runs in constant-quality mode (CRF 22) with a
bitrate ceiling that follows the picture size (9 Mbps at 960x720). Knobs: `TOTV_STREAM_{N64,PSP,PS2}_SIZE=WxH`, `TOTV_STREAM_WIDTH/HEIGHT` (all cores),
`TOTV_STREAM_{N64,PSP,PS2}_RES`, `TOTV_STREAM_KBPS` (ceiling), `TOTV_STREAM_CRF`, `TOTV_STREAM_X264_THREADS`, `TOTV_STREAM_X264_PRESET`; the pictures keep the
aspect the core reports (black bars, never a stretch). A session whose host time per frame stays above 12 ms (p95) says so in the log and in
`GET /v1/sessions/{id}` (`overBudget`, `suggestQuality`). Numbers and what was not measured: [docs/DEVLOG.md](docs/DEVLOG.md), "Picture quality".

## Security and isolation (honest summary)

Each session runs the core in a separate child process (`retro-emu`): ROM and save are read first, then all capabilities
are dropped, a user + network namespace (empty) is entered, and a seccomp-bpf deny-list (no sockets, ptrace, io_uring,
bpf, mount, kill of others, ...) plus Landlock (read-only library dirs only) are applied **before** the core's code runs.
The parent verifies the result through `/proc` instead of trusting the child. Per-session cgroup limits, a PID
namespace, closed GPU device descriptors and a dedicated uid unit are available.

Limits you should know about:

- The seccomp filter is a deny-list, not an allow-list.
- GPU cores (N64/PSP) must keep GPU device access, so a compromised core can still reach the kernel GPU driver
  interface; there is no per-process VRAM limit. For untrusted ROMs or multi-user setups this is not enough (a microVM
  with its own GPU would be needed, not done).
- Cores are trusted software built by you; a malicious core `.so` is outside the threat model.
- Several security reviews were done by the author with automated assistance; no independent audit exists.

Details and the full list: [docs/DEVLOG.md](docs/DEVLOG.md) ("Isolation", "Hardening", "GPU isolation"). Report
vulnerabilities as described in [SECURITY.md](SECURITY.md).

## What is NOT proved

- SNES, N64 and PSP were proved only on the author's machine (one Linux PC, one NVIDIA GPU). **PS2 is a prototype that has never run a game**: there is no PS2 BIOS or image of a game on the author's machine (what was and was not proved: [docs/DEVLOG.md](docs/DEVLOG.md), "PS2").
- Docker with GPU, a microVM, and a published image: not done. The Docker image was built and run without GPU only.
- Glass-to-glass latency on a real TV, Android WebRTC client interoperability, 30-minute stability, packet loss and
  bitrate adaptation: not measured. Only a pion client was used as the peer.
- Commercial games were exercised only up to title/intro screens (PSP) or a few seconds of play; compatibility is unknown.
- Real Totv integration was tested against a fake Totv built from reading the Totv routes.

See [ROADMAP.md](ROADMAP.md).

## License

GPL-3.0-or-later ([LICENSE](LICENSE)). Third-party components and their licenses: [NOTICE.md](NOTICE.md). The project
is gratuitous and non-commercial in intent, but the GPL itself does not forbid commercial use of this code; the
"non-commercial" restriction comes from the snes9x core only.
