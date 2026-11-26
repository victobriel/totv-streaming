# Integration contract

What a host program (the Totv media server, or any program that plays its role) must speak to drive this component. It is
derived from the code (`control.go`, `session.go`, `input.go`, `savezip.go`, `totv.go`, `rtc.go`); where this text and the code
disagree, the code wins. The full list of environment variables and the operational details are in [DEVLOG.md](DEVLOG.md).

## Transport and authentication

- The control API is plain HTTP over a **Unix socket** (`TOTV_STREAM_SOCK`). There is no TCP listener for it.
- Every request needs `Authorization: Bearer <secret>`. The secret is the content of the file `TOTV_STREAM_SECRET_FILE`
  (whitespace trimmed, re-read on every request, so it can be rotated without a restart). It is compared in constant time.
  A missing or wrong secret gives `401 {"error":"unauthorized"}`. Keep the socket and the secret file readable only by the
  host program's user.
- Bodies are JSON. Errors are `{"error":"<code>"}`. Request and response bodies are never logged.

## Routes

| Route | Meaning |
|---|---|
| `GET /v1/health` | `{version, encoder:"x264", gpu: null or {api,renderer,version}, cores:{snes:[..],n64:[..],psp:[..]}, isolation:{...}, limits:{...}}`. Core names are `server_<name>`; GPU cores are listed only when an EGL/GL context could be created at start. |
| `GET /v1/sessions` | `[{id, state}, ...]` |
| `POST /v1/sessions` | Creates a session (body below). `201 {"id"}`. |
| `GET /v1/sessions/{id}` | `{state, playedMs, peers:[{peerId, slot, state}], stats:{...}, quality, aspect, output, kbpsCeiling, fps, overBudget, suggestQuality}` (counters and latency percentiles; `quality` `high`/`safe`, `aspect` the current picture placement (below), `output` `WxH` of the stream, `kbpsCeiling` the VBV cap, `fps` the core's frame rate; `overBudget` true when the host's per-frame time p95 stayed above `TOTV_STREAM_HOSTMS_P95_X10`/10 ms (default 12) for 3 windows of 120 frames, and then `suggestQuality` is `"safe"`: the Totv can offer to restart the game lower). `404 not_found` if unknown. |
| `PATCH /v1/sessions/{id}` | Changes a live session. Body `{"aspect":"auto\|4:3\|16:9\|fill\|pixel"}` (and nothing else) -> `200 {"ok":true,"aspect":"<mode>"}`; `400 bad_request` for an unknown mode, a missing or extra field, an empty or non-JSON body; `404 not_found`; `401` like every route. See "Picture placement". |
| `POST /v1/sessions/{id}/signal` | WebRTC signalling (below). |
| `POST /v1/sessions/{id}/peers` | Reserves a player slot (multi-peer, below). `201 {"peerId","slot"}`. |
| `DELETE /v1/sessions/{id}/peers/{peerId}` | Drops that one peer (below). `200 {"ok":true}`, `404 peer_not_found`. |
| `DELETE /v1/sessions/{id}` | Stops the session, stores the save, answers `200 {"ok":true,"saved":<bool>}`; `502 save_failed` if the save could not be stored. |

Create body (limit 16 KiB):

```
{ "sessionId": "[A-Za-z0-9_-]{1,64}", "itemId": <int > 0>, "mediaId": <int > 0>,
  "platform": "snes|n64|psp", "core": "server_<name>", "token": "<session token>",
  "serverUrl": "http://127.0.0.1:<port>[/prefix]", "iceHost": "<IPv4 the client can reach>",
  "advertiseIps": ["<IPv4>", ...] /* optional, 0..2 literal IPv4s, see "Extra ICE addresses" */,
  "aspect": "auto|4:3|16:9|fill|pixel" /* optional, default "auto": the initial picture placement, see below */,
  "quality": "high|safe" /* optional; default TOTV_STREAM_QUALITY (high). "safe" = 640x480 (PSP 848x480), native render scale, 4 Mbps ceiling */ }
```

Simultaneous sessions: up to `TOTV_STREAM_MAX_SESSIONS` (default **2**, clamped to 1..4; `maxSessions` in `GET /v1/health`). Every session is its own emulator process, x264 encoder
and per-session cgroup, with its own temporary directories (`ses-*`, `rw-*`) and save; all of them share the GPU and ONE UDP socket/ICE mux on `TOTV_STREAM_UDP_PORT` (the ICE ufrag
tells the sessions, and the peers inside a session, apart). Input of a session reaches only that session. Ending one never touches the others.

Errors: `400 bad_request` / `unknown_core`, `409 busy` (session limit, `TOTV_STREAM_MAX_SESSIONS`; the body also carries `"reason":"max_sessions"` and `"max":N`, additive: an old client only reads `error`) / `exists`, `502`
(`system_assets_missing`, download or start failures), `503 isolation_unavailable` (no seccomp/Landlock and no development
override). `platform` must match the core.

Session `state` values: `starting`, `waiting_signal`, `connecting`, `playing`, `disconnected`, `stopping`, `stopped`.
There is no event stream: the host polls `GET /v1/sessions/{id}`. A session nobody signals is ended after
`TOTV_STREAM_NOSIGNAL_SEC`; one whose peer is gone is ended after `TOTV_STREAM_DISCONNECT_SEC`; one whose core stops
producing video is ended without saving (`TOTV_STREAM_NOVIDEO_SEC`).

## Extra ICE addresses (`advertiseIps`)

Optional list of 0 to 2 literal IPv4 addresses (for a host with several networks, e.g. LAN and VPN). The answer then carries one host candidate for `iceHost` **and** one for each
address in the list (all on `TOTV_STREAM_UDP_PORT`); the UDP socket stays bound to `iceHost:UDPPort` only, so the extra addresses must reach that socket (routing/NAT is the operator's).
Absent or empty = only `iceHost`, as before. `400 bad_request` when it is not a list of at most 2 strings, or an entry is not a literal IPv4, or is loopback, unspecified (`0.0.0.0`),
multicast, link-local (`169.254/16`) or broadcast. Duplicates (and `iceHost` itself) are collapsed.

## Multi-peer (players per session)

A session can have several WebRTC peers, one per player **slot** (0 = the owner, 1...). Up to `TOTV_STREAM_MAX_PEERS` peers (default **2**, at most 4); a platform also caps it:
`GET /v1/health` reports `multiplayer: {"snes": 2}` (platforms with more than one player and how many, already capped by `MaxPeers`) and `maxPeers`. SNES = 2 players in this release;
N64, PSP and PS2 = 1 (no multitap: 3-5 players are not implemented).

- **One encoder, one packetizer, one video track and one audio track** are shared; the same RTP is sent to every peer's PeerConnection (CPU grows by the extra SRTP/NACK work only, see DEVLOG).
  `streaming` = at least one peer connected. A **keyframe** is forced when a peer connects and on a PLI/FIR from any peer. The playout-delay extension id is the last one negotiated (browsers use the same id).
- `POST /v1/sessions/{id}/peers` body `{"slot": N}` -> `201 {"peerId":"p...","slot":N}`. Errors: `404 not_found` (session), `400 bad_request` (missing/non-integer slot, `slot` not in 0..3 or beyond
  the slots of the session), `409 not_multiplayer` (slot > 0 on a one-player platform), `409 room_full` (all slots taken), `409 slot_taken`. A peer starts as `waiting_signal`.
- **Signalling per peer:** `POST /v1/sessions/{id}/signal` accepts an optional `"peerId"` next to `type`/`sdp`/`candidate`. **Absent = the peer of slot 0**, created on the spot if the host never called `POST .../peers`:
  the single-player flow (offer, candidates, no peers route) works unchanged. An unknown `peerId` is `404 peer_not_found`; a second offer for the same peer is `400 already_negotiated`.
- `DELETE /v1/sessions/{id}/peers/{peerId}` closes only that peer: its buttons are released, its slot is free again. **Deleting slot 0 does not end the session**; the host calls `DELETE /v1/sessions/{id}` for that.
- Peer `state`: `waiting_signal` (reserved, no offer yet), `connecting`, `playing`, `disconnected`. Session `state` is `playing` while at least one peer plays. Timers are per peer: a slot without an offer
  after `TOTV_STREAM_NOSIGNAL_SEC`, or a peer disconnected for `TOTV_STREAM_DISCONNECT_SEC`, is freed. The session itself ends (as before) when **no** peer has been connected for `TOTV_STREAM_DISCONNECT_SEC`
  (or, as before, nobody ever signalled for `TOTV_STREAM_NOSIGNAL_SEC`).
- **Input:** each peer's DataChannel feeds its own controller: the component **overwrites byte 11 (`port`) with the peer's slot** (slot 0 -> port 0, slot 1 -> port 1); whatever the client writes there is ignored
  (this also applies to the one-player flow: `port` is always 0 there). `seq` anti-repetition is kept per peer.

## Picture placement (`aspect`)

The stream has a **fixed** size for the whole session (the encoder is never restarted): in `high`, N64 and PS2 are 1280x720 (16:9; before: 960x720), the PSP 960x544, SNES as configured; `safe` keeps 640x480 (PSP 848x480).
`aspect` only says how the core's picture is placed in that frame, and can be changed live with `PATCH /v1/sessions/{id}` (the host's stdin message `kind 4`, `[4, mode, 0, 0]`, mode 0..4 in the order below;
it takes effect on the next frame and clears the old bars):

| mode | picture |
|---|---|
| `auto` (default) | the display aspect the core reports, centered with black bars (a 4:3 N64/PS2 picture is a 960x720 pillarbox inside 1280x720; a difference under 3% fills the frame) |
| `4:3` | forced 4:3, bars at the sides (also for a core that reports another aspect) |
| `16:9` | stretched over the whole frame (distorts a 4:3 picture) |
| `fill` | covers the whole frame keeping the core's aspect: the excess (top and bottom of a 4:3 picture) is cropped, centered |
| `pixel` | whole-number scale (1x, 2x, 3x...) with the core's aspect applied to the width, centered, black bars, nearest-neighbour; if not even 1x fits (PS2 at 2x renders 1280x896) it behaves like `auto` |

The client does not need to know: the video track always has the same size. H.264 stays Constrained Baseline; the SDP announces the level of the real picture (`profile-level-id=42e020`, level 3.2, for 1280x720 at 50 or 60 fps; `level-asymmetry-allowed=1`).
The bitrate ceiling follows the pixels (12 Mbps at 1280x720, 9 Mbps at 960x720, under the 20 Mbps of level 3.2).

## Signalling

`POST /v1/sessions/{id}/signal`, body limit 64 KiB (`413 too_large`):

- `{"type":"offer","sdp":"..."}` gives `200 {"type":"answer","sdp":"..."}`. The answer already holds the only ICE candidate
  (host candidate on `iceHost`, UDP port `TOTV_STREAM_UDP_PORT`, default 8422, the same port for every session; no STUN, no TURN, no mDNS).
- `{"candidate":{...}}` gives `200 {"ok":true}`; `{"candidate":null}` is accepted and ignored.
- Anything else: `400 bad_request`.

Media: H.264 Baseline without B-frames (x264) and Opus, both from the component to the client, with the playout-delay RTP
header extension at 0/0 when negotiated.

## Input

The client opens a DataChannel labelled **`input`** (any other label is closed), unordered. Each message is one packet of
exactly **12 bytes**, big-endian:

| Byte | Field | Notes |
|---|---|---|
| 0 | version | must be 1 |
| 1-2 | seq | uint16; a packet not strictly newer than the last accepted (16-bit wrap-around, `int16(seq-last) > 0`) is dropped |
| 3-6 | buttons | uint32 bitmask: bit 0 dpad_up, 1 dpad_down, 2 dpad_left, 3 dpad_right, 4 a, 5 b, 6 x, 7 y, 8 l1, 9 r1, 10 l2, 11 r2, 12 l3, 13 r3, 14 start, 15 select, 16 guide (ignored); 17-31 must be 0 |
| 7-10 | axes | 4 x int8: left X, left Y, right X, right Y (used by N64, PSP and PS2; PS2 uses both sticks) |
| 11 | port | ignored: the component writes the slot of the peer that sent the packet (see Multi-peer) |

`a/b/x/y` are physical positions of an Xbox-style pad (a south, b east, x west, y north). Packets of another length or
version are dropped and counted (`inputBad`, `inputOldDropped` in the status). Clients are expected to
resend the current state every ~16 ms.

## ROM and saves (what the component asks of the host)

The component talks back to the host program at `serverUrl` (loopback), with `Authorization: Bearer <token>` (the session
token from the create body; it never reaches the emulator process):

- `GET /api/games/rom/{mediaId}` downloads the ROM (limit 64 MiB; larger for PSP discs). `404` ends the start.
- `GET /api/games/{itemId}/sram` downloads the save (`404` = no save yet; any other error stops the start rather than begin
  from an empty save).
- `PUT /api/games/{itemId}/sram?core=server_<name>&mediaId=<id>` uploads the save (`application/octet-stream`), when the
  session ends and, for PSP, periodically while playing.

SNES and N64 saves are the raw SRAM image (limit 2 MiB on download). A PSP save is a **directory**, exchanged as one **zip**
(up to 64 MiB packed, 256 MiB unpacked, 4096 entries): classic zip (no ZIP64), methods stored or deflate, no encryption,
names relative with `/` only, all below `PSP/SAVEDATA/`, no `..`, no absolute path, no drive letter, no backslash or control
character, no symlinks, no duplicate names. The component validates every zip it receives before writing a byte to disk and
only ever packs `PSP/SAVEDATA/`.

## PS2 (prototype): what the Totv must know

- **Core:** `server_pcsx2`, platform `ps2`. Offered in `/v1/health` `cores.ps2` only when a valid BIOS is installed. `health.ps2` is `{bios, reason?, bios_dir_set, bios_files, bios_valid, max_iso_mb, big_tmp}`
  (`reason` is `bios_missing` or `bios_invalid` when `bios` is false); it is absent when no PS2 core is installed. `ps2.bios=false` means every PS2 session will be refused.
- **Errors of `POST /v1/sessions`** (HTTP 502, before anything is downloaded): `bios_missing` (no directory, empty, or no candidate files), `bios_invalid` (files exist but none has a valid
  PS2 ROMDIR: a PS1 BIOS, a truncated dump). Also `rom_invalid` (zip/gzip image), `rom_download_failed` (over `max_iso_mb`, an HTTP error, a cut or timed-out download; the 502 body carries a sanitized `detail`), `rom_too_large_tmp` (no `TOTV_STREAM_BIG_TMP`, no disk fallback and the RAM tmpfs cannot hold the image), `no_space`, `save_load_failed`.
- **Image:** `GET /api/games/rom/{mediaId}` serves the ISO/CHD/CSO/ZSO as a file (up to 9 GiB by default); the start can take **minutes** (the component copies it to disk; `POST /v1/sessions`
  may block up to 10 minutes: the Totv's client timeout must allow it). The server must send `Content-Length` (the free space is checked before the first byte).
- **Save:** `GET/PUT /api/games/{itemId}/sram` carry a **zip** with exactly these entries and no others: `pcsx2/memcards/Mcd001.ps2` and `pcsx2/memcards/Mcd002.ps2` (raw memory cards, 8650752 bytes each; the
  component accepts up to 72 MiB per card, 80 MiB unpacked, 64 MiB packed). The Totv needs a `zipSave` type for `ps2` in `server/games/saves.js` that accepts those two names (and refuses everything else
  or leaves it out), with the same zip rules as the PSP's. A fresh session writes two blank (0xFF) cards, so the first stop uploads a ~17 KB zip even if nothing was played.
- **BIOS:** nothing for the Totv to send: the operator configures `TOTV_STREAM_PS2_BIOS_DIR` on the component. The Totv should show `bios_missing`/`bios_invalid` to the user as "PS2 BIOS not installed".
- **Input:** the 12-byte packet is unchanged. Face buttons `a/b/x/y` = Cross/Circle/Square/Triangle, `l1/r1/l2/r2/l3/r3`, `start`, `select`, d-pad; axes 0,1 = left stick, axes 2,3 = right stick.

## Limits worth knowing

Body limits above; one session per core process; per-session cgroup, writable-directory quota and file-size limits
(`TOTV_STREAM_SESSION_PIDS`, `_SESSION_MEM_MB`, `_RW_MAX_MB`, `_RW_MAX_FILES`, `_MAX_FILE_MB`). The `TOTV_STREAM_*`
variable names are part of the contract and must not be renamed.
