# totv-streaming (GPL-3.0-or-later). Component image: Go server + retro-emu (C host) + x264/opus. NO libretro core, NO ROM, NO BIOS inside:
# cores are mounted at /opt/cores (scripts/fetch-cores.sh builds them against THIS Debian, see README "Docker").
# Debian 13 (trixie), not 12: retro-emu needs the Landlock ABI 4/5 kernel headers (linux-libc-dev >= 6.7), bookworm's 6.1 lacks them.
# Bases are pinned by digest (resolved with `docker buildx imagetools inspect <tag>`); bump them on purpose.
ARG DEBIAN=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
ARG DEBIAN_FULL=debian:trixie@sha256:9c6e381421d0f48d1b8133e573f33c185ee16cbf505eda9c4a8a4791b25ea8a6
ARG GOLANG=golang:1.24-trixie@sha256:5835f052b784aa39f2fe9070def3568605c8bc3fcd810f10402066348b61e716

# ---- build: Go server + retro-emu (C; HW_RENDER compiled in because the EGL/GL headers exist; libEGL is dlopen()ed, never linked)
FROM ${GOLANG} AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc make pkg-config libc6-dev libx264-dev libopus-dev libegl-dev libgl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make bin/retro-emu && CGO_ENABLED=1 go build -trimpath -ldflags '-s -w' -o bin/totv-streaming . \
    && strip bin/retro-emu \
    && test -n "$(strings bin/retro-emu | grep -m1 'libEGL.so.1')"

# ---- corebuild: toolchain used ONLY by scripts/fetch-cores.sh (never part of the runtime image). Same Debian as the runtime
# so the cores link against the glibc/libstdc++/libGL they will run with.
FROM ${DEBIAN_FULL} AS corebuild
RUN apt-get update && apt-get install -y --no-install-recommends build-essential git ca-certificates cmake nasm pkg-config python3 \
    libegl-dev libgl-dev libx11-dev zlib1g-dev && rm -rf /var/lib/apt/lists/* \
    && mkdir /work && chmod 1777 /work

# ---- runtime
FROM ${DEBIAN} AS runtime
# MESA=1 adds the Mesa EGL/GL drivers (Intel/AMD, VAAPI side): NOT TESTED, see README. NVIDIA needs none of it (the NVIDIA Container
# Toolkit injects libEGL_nvidia/libGLX_nvidia/libnvidia-* at start; only the glvnd dispatch libs below come from the image).
ARG MESA=0
RUN apt-get update && apt-get install -y --no-install-recommends libx264-164 libopus0 libstdc++6 libegl1 libgl1 libx11-6 \
    && if [ "$MESA" = 1 ]; then apt-get install -y --no-install-recommends mesa-va-drivers; else \
       # libegl1/libgl1 "Depend" on Mesa (+ LLVM, ~200 MB) when no other glvnd vendor is installed; the NVIDIA vendor comes from the toolkit, so drop Mesa\
       dpkg --purge --force-depends libegl-mesa0 libglx-mesa0 libgl1-mesa-dri mesa-libgallium libllvm19 libgbm1 libdrm-amdgpu1 libdrm-intel1 \
         libsensors5 libsensors-config libedit2 libelf1t64 libwayland-client0 libwayland-server0 libxcb-dri3-0 libxcb-present0 libxcb-randr0 \
         libxcb-shm0 libxcb-sync1 libxcb-xfixes0 libx11-xcb1 libxml2 libxcb-glx0; fi \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 totv && useradd -u 1000 -g 1000 -M -d /nonexistent -s /usr/sbin/nologin totv \
    && mkdir -p /opt/cores /run/totv-stream /run/totv-stream-secret /run/totv-tmp /run/totv-rw /usr/share/glvnd/egl_vendor.d \
    && chown 1000:1000 /run/totv-stream /run/totv-tmp /run/totv-rw \
    && printf '{"file_format_version":"1.0.0","ICD":{"library_path":"libEGL_nvidia.so.0"}}\n' > /usr/share/glvnd/egl_vendor.d/10_nvidia.json
COPY --from=build /src/bin/totv-streaming /src/bin/retro-emu /usr/local/bin/
ENV TOTV_STREAM_SOCK=/run/totv-stream/control.sock \
    TOTV_STREAM_SECRET_FILE=/run/totv-stream-secret/retro-stream.secret \
    TOTV_STREAM_CORES=/opt/cores TOTV_STREAM_EMU=/usr/local/bin/retro-emu \
    TOTV_STREAM_TMP=/run/totv-tmp TOTV_STREAM_RW_TMP=/run/totv-rw TOTV_STREAM_UDP_PORT=8422 \
    NVIDIA_VISIBLE_DEVICES=all NVIDIA_DRIVER_CAPABILITIES=graphics,utility HOME=/nonexistent
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/totv-streaming"]
