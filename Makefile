# totv-streaming (GPL-3.0-or-later)
.PHONY: all emu build test clean latency snes
all: emu build
emu: bin/retro-emu bin/glcore_libretro.so bin/pspcore_libretro.so bin/ps2core_libretro.so bin/testcore_libretro.so bin/testcore565_libretro.so bin/testcore1555_libretro.so bin/threadcore_libretro.so bin/attackcore_libretro.so bin/glhogt_libretro.so bin/glhogm_libretro.so
# GPU (HW_RENDER) support is compiled in when the EGL/GL headers exist; libEGL is dlopen()ed at run time, never linked.
HAVE_EGL := $(shell test -e /usr/include/EGL/egl.h -a -e /usr/include/GL/glext.h && echo -DHAVE_EGL)
bin/retro-emu: emu/host.c emu/glhost.h emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall $(HAVE_EGL) -o $@ emu/host.c -ldl
bin/glcore_libretro.so: emu/testcore/glcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -o $@ emu/testcore/glcore.c -lm
bin/glhogt_libretro.so: emu/testcore/glcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DGL_HOG=1 -o $@ emu/testcore/glcore.c -lm -lpthread
bin/glhogm_libretro.so: emu/testcore/glcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DGL_HOG=2 -o $@ emu/testcore/glcore.c -lm -lpthread
bin/testcore_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -o $@ emu/testcore/testcore.c -lm
bin/pspcore_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DTC_FULLPATH -o $@ emu/testcore/testcore.c -lm -lrt
bin/ps2core_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DTC_PS2 -o $@ emu/testcore/testcore.c -lm -lrt
bin/threadcore_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DTC_THREAD -o $@ emu/testcore/testcore.c -lm -lpthread
bin/attackcore_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DTC_ATTACK -o $@ emu/testcore/testcore.c -lm
bin/testcore565_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DTC_FMT=565 -o $@ emu/testcore/testcore.c -lm
bin/testcore1555_libretro.so: emu/testcore/testcore.c emu/libretro.h
	@mkdir -p bin && gcc -O2 -Wall -shared -fPIC -DTC_FMT=1555 -o $@ emu/testcore/testcore.c -lm
build: emu
	go build -o bin/totv-streaming .
test: emu
	go vet ./... && go test -race -count=1 ./...
latency: emu
	PROBE=1 go test -count=1 -run TestLatencyProbe -v ./...
clean:
	rm -rf bin

snes: emu
	SNES=1 go test -race -count=1 -run TestSNESKeepsStreaming -v .
	SNES=1 go test -count=1 -run TestRealSNES -v ./...

# GPU path without any ROM (triangle) + latency through it
gl: emu
	go test -count=1 -run 'TestGLCore|TestHostOptions|TestN64Button' -v .
	PROBE=1 go test -count=1 -run TestLatencyProbeGL -v .

# real N64 (needs cores/mupen64plus_next_libretro.so and a ROM outside the repo): N64_ROM, N64_SECS, N64_RES=native|2x, N64_READBACK=sync|pbo, N64_OUT
n64: emu
	N64=1 go test -count=1 -run TestRealN64 -v -timeout 300s .
.PHONY: gl n64

# real PSP (needs cores/ppsspp_libretro.so, cores/system/PPSSPP (scripts/psp-assets.sh) and PSP_ROM=<a homebrew or game outside the repo>)
psp: emu
	PSP=1 go test -count=1 -run 'TestRealPSP$$' -v -timeout 300s .
	PSP=1 go test -count=1 -run TestRealPSPLatency -v -timeout 300s .
.PHONY: psp

# real PS2 (needs cores/pcsx2_libretro.so, scripts/fetch-cores.sh builds it). Without PS2_ROM/PS2_BIOS_DIR the target runs the synthetic-BIOS sandbox
# test; with PS2_ROM=<image outside the repo> and PS2_BIOS_DIR=<your own BIOS dir> it runs the real game test.
ps2: emu
	PS2=1 go test -count=1 -run 'TestRealPS2' -v -timeout 600s .
.PHONY: ps2
