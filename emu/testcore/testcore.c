/* SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (C) 2026 victobriel */
/* testcore: a tiny libretro core that needs NO ROM (any file works). GPL-3.0-or-later.
 * Draws a moving square and a frame counter bar, makes a 440 Hz tone, and:
 *  - while retropad A is held, the whole screen is white and the tone jumps to 880 Hz (latency probe);
 *  - save RAM (8 KiB): bytes 0..3 count the sessions that loaded this game (save/load test). */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <math.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <string.h>
#include <stdio.h>
#include <sys/stat.h>
#include <sys/mman.h>
#include <stdlib.h>
#include <signal.h>
#include <pthread.h>
#include "../libretro.h"
#define W 320
#define H 240
#define RATE 48000.0
static retro_environment_t env; static retro_video_refresh_t vid; static retro_audio_sample_batch_t aud; static retro_input_poll_t poll_; static retro_input_state_t inp;
#ifdef TC_FULLPATH
#define TC_NOSRAM 1
#else
#define TC_NOSRAM 0
#endif
static uint32_t fb[W * H]; static uint8_t sram[8192]; static unsigned frame; static double phase;
/* Like snes9x (which prints "Map_LoROMMap" at exit), write junk to fd 1 when the process exits: the host must keep the
 * protocol on a private fd so this can never be read as a frame. */
__attribute__((destructor)) static void noisy_exit(void) { ssize_t r = write(1, "Map_LoROMMap\n", 13); (void)r; }
void retro_set_environment(retro_environment_t e) { env = e; }
void retro_set_video_refresh(retro_video_refresh_t v) { vid = v; }
void retro_set_audio_sample(retro_audio_sample_t a) { (void)a; }
void retro_set_audio_sample_batch(retro_audio_sample_batch_t a) { aud = a; }
void retro_set_input_poll(retro_input_poll_t p) { poll_ = p; }
void retro_set_input_state(retro_input_state_t i) { inp = i; }
#ifndef TC_FMT
#define TC_FMT 8888
#endif
#if TC_FMT == 565
#define TC_PF RETRO_PIXEL_FORMAT_RGB565
#elif TC_FMT == 1555
#define TC_PF RETRO_PIXEL_FORMAT_0RGB1555
#else
#define TC_PF RETRO_PIXEL_FORMAT_XRGB8888
#endif
#if TC_FMT == 565 || TC_FMT == 1555
static uint16_t fb16[W * H];
#endif
void retro_init(void) { unsigned f = TC_PF; env(RETRO_ENVIRONMENT_SET_PIXEL_FORMAT, &f); }
void retro_deinit(void) {}
unsigned retro_api_version(void) { return RETRO_API_VERSION; }
void retro_get_system_info(struct retro_system_info *i) { memset(i, 0, sizeof *i); i->library_name = "testcore"; i->library_version = "1"; i->valid_extensions = "*";
#ifdef TC_FULLPATH
    i->need_fullpath = true; i->block_extract = true;
#endif
}
void retro_get_system_av_info(struct retro_system_av_info *i) { memset(i, 0, sizeof *i); i->geometry.base_width = i->geometry.max_width = W; i->geometry.base_height = i->geometry.max_height = H; i->geometry.aspect_ratio = 4.0f / 3; i->timing.fps = 60.0; i->timing.sample_rate = RATE; }
/* Sandbox probe for the tests. The constructor runs at dlopen(), which the host does BEFORE the sandbox: if /etc/passwd
 * opens there, the order is proven. A game whose data starts with "SBXPROBE" then makes retro_load_game (which the host
 * runs AFTER the sandbox) fail unless: the early open worked, the late open, a UDP socket() and ptrace() are all denied. */
static int pre_open = -1;
__attribute__((constructor)) static void early(void) { pre_open = open("/etc/passwd", O_RDONLY | O_CLOEXEC); if (pre_open >= 0) close(pre_open); }
#ifdef TC_THREAD
/* a driver that starts a helper thread while it initialises (before the sandbox): the host must refuse to go on (Landlock is per thread) */
static void *idle_thread(void *a) { (void)a; for (;;) sleep(1000); return 0; }
__attribute__((constructor)) static void spawn_thread(void) { pthread_t t; pthread_create(&t, 0, idle_thread, 0); }
#endif
#ifdef TC_ATTACK
/* hostile core (build with -DTC_ATTACK): tries to make the PARENT receive a SIGKILL through its own stdin pipe (fd 0): F_SETOWN to the parent, F_SETSIG
 * SIGKILL, O_ASYNC; the parent's next write of an input message then kills it. ATTACK_CTOR=1 does it in a constructor (before the sandbox),
 * ATTACK_LOAD=1 in retro_load_game (inside the sandbox). Prints what the kernel answered on stderr; the game loads either way. */
static void attack(const char *when) {
    errno = 0; int a = fcntl(0, F_SETOWN, getppid()), ea = errno;
    errno = 0; int b = fcntl(0, 10 /* F_SETSIG */, SIGKILL), eb = errno;
    int fl = fcntl(0, F_GETFL); errno = 0; int c = fl >= 0 ? fcntl(0, F_SETFL, fl | O_ASYNC) : -1, ec = errno;
    int armed = a == 0 && b == 0 && c == 0;
    dprintf(2, "attack(%s): F_SETOWN=%d(%d) F_SETSIG=%d(%d) O_ASYNC=%d(%d) armed=%d\n", when, a, ea, b, eb, c, ec, armed);
}
__attribute__((constructor)) static void attack_ctor(void) { if (getenv("ATTACK_CTOR")) attack("ctor"); }
#endif
#ifdef TC_FULLPATH
/* PSP-like core (build with -DTC_FULLPATH): need_fullpath, like PPSSPP. It must receive a PATH and no data, read the file itself,
 * keep a "memory stick" under GET_SAVE_DIRECTORY (PSP/SAVEDATA/TEST00/DATA.BIN, a counter that grows on every session) and be unable
 * to write anywhere else. The screen is green when every probe held and red otherwise. */
static int probe_ok; static unsigned long long rom_bytes; static unsigned save_count;
/* bytes 8..15 of the "ROM" pick a misbehaviour for the component's tests: CRASHSAV (half-writes the save and aborts at frame 40), FSIZETST
 * (the core must be unable to grow a file past the host's RLIMIT_FSIZE: SIGXFSZ ignored, the ftruncate must fail with EFBIG), FILLRW
 * (writes 3 MiB into the memory stick, below the file limit, to trip the per-session quota) */
static char mode[9]; static char savepath[4400];
#define PFAIL() (dprintf(2, "pspcore probe failed at line %d (errno %d)\n", __LINE__, errno), 0)
static int fullpath_probe(const struct retro_game_info *g) {
    if (!g || !g->path || g->data) return PFAIL(); /* the host must NOT have loaded the game into memory */
    FILE *f = fopen(g->path, "rb"); if (!f) return PFAIL();
    char magic[8] = ""; unsigned char b[65536]; size_t n; int first = 1;
    while ((n = fread(b, 1, sizeof b, f)) > 0) { if (first) { memcpy(magic, b, n < 8 ? n : 8); first = 0; } rom_bytes += n; }
    fclose(f);
    if (memcmp(magic, "PSPTEST1", 8)) return PFAIL();
    { FILE *m = fopen(g->path, "rb"); if (m) { if (fread(mode, 1, 8, m) != 8) mode[0] = 0; if (fread(mode, 1, 8, m) != 8) mode[0] = 0; fclose(m); } }
    char dir[4096]; snprintf(dir, sizeof dir, "%s", g->path); char *sl = strrchr(dir, '/'); if (!sl) return PFAIL(); *sl = 0;
    char p[4400];
    FILE *w = fopen(g->path, "r+b"); if (w) { fclose(w); return PFAIL(); }             /* the ROM is read-only */
    snprintf(p, sizeof p, "%s/evil", dir); w = fopen(p, "wb"); if (w) { fclose(w); return PFAIL(); } /* nothing may be created next to it */
    int e = open("/etc/passwd", O_RDONLY); if (e >= 0) { close(e); return PFAIL(); }
    /* PPSSPP builds its RAM as a shm file: shm_open must work, and it must be a PRIVATE /dev/shm (the host's marker file is invisible) */
    int sf = shm_open("/totv_probe", O_RDWR | O_CREAT | O_EXCL, 0600); if (sf < 0) return PFAIL();
    shm_unlink("/totv_probe"); if (ftruncate(sf, 64 << 20)) return PFAIL();
    void *mm = mmap(NULL, 64 << 20, PROT_READ | PROT_WRITE, MAP_SHARED, sf, 0); if (mm == MAP_FAILED) return PFAIL(); ((char *)mm)[(64 << 20) - 1] = 1; munmap(mm, 64 << 20); close(sf);
    int hm = open("/dev/shm/totv-host-marker", O_RDONLY); if (hm >= 0) { close(hm); return PFAIL(); }
    const char *sd = NULL; if (!env(RETRO_ENVIRONMENT_GET_SAVE_DIRECTORY, &sd) || !sd) return PFAIL();
    snprintf(p, sizeof p, "%s/../escape", sd); w = fopen(p, "wb"); if (w) { fclose(w); return PFAIL(); } /* ".." out of the save dir */
    snprintf(p, sizeof p, "%s/lnk", sd); if (symlink("/etc/passwd", p) == 0) return PFAIL();          /* no symlinks in it either */
    snprintf(p, sizeof p, "%s/PSP", sd); mkdir(p, 0700);
    snprintf(p, sizeof p, "%s/PSP/SAVEDATA", sd); mkdir(p, 0700);
    snprintf(p, sizeof p, "%s/PSP/SAVEDATA/TEST00", sd); mkdir(p, 0700);
    snprintf(p, sizeof p, "%s/PSP/SYSTEM", sd); mkdir(p, 0700); snprintf(p, sizeof p, "%s/PSP/SYSTEM/CACHE", sd); mkdir(p, 0700);
    snprintf(p, sizeof p, "%s/PSP/SYSTEM/CACHE/shaders.bin", sd); w = fopen(p, "wb"); if (!w) return PFAIL(); fputs("cache", w); fclose(w); /* a shader-cache-like file: must not end in the save package */
    snprintf(p, sizeof p, "%s/PSP/SAVEDATA/TEST00/DATA.BIN", sd);
    w = fopen(p, "rb"); if (w) { if (fread(&save_count, 4, 1, w) != 1) save_count = 0; fclose(w); }
    save_count++;
    w = fopen(p, "wb"); if (!w) return PFAIL(); fwrite(&save_count, 4, 1, w); fclose(w);
    snprintf(savepath, sizeof savepath, "%s", p);
    if (!strcmp(mode, "FSIZETST")) {
        signal(SIGXFSZ, SIG_IGN);
        snprintf(p, sizeof p, "%s/PSP/big.bin", sd); int bf = open(p, O_WRONLY | O_CREAT, 0600); if (bf < 0) return PFAIL();
        errno = 0; if (ftruncate(bf, 256LL << 20) == 0 || errno != EFBIG) return PFAIL(); close(bf);
    }
    if (!strcmp(mode, "FILLINO")) { /* 300 empty files: no bytes at all, only inodes (the entry quota) */
        for (int i = 0; i < 300; i++) { snprintf(p, sizeof p, "%s/PSP/e%d", sd, i); int bf = open(p, O_WRONLY | O_CREAT, 0600); if (bf < 0) return PFAIL(); close(bf); }
    }
    if (!strcmp(mode, "FILLRW")) {
        snprintf(p, sizeof p, "%s/PSP/fill.bin", sd); int bf = open(p, O_WRONLY | O_CREAT, 0600); if (bf < 0) return PFAIL();
        static char blk[65536]; for (int i = 0; i < 48; i++) if (write(bf, blk, sizeof blk) != (ssize_t)sizeof blk) return PFAIL(); close(bf);
    }
    return 1;
}
#endif
bool retro_load_game(const struct retro_game_info *g) {
#ifdef TC_ATTACK
    if (getenv("ATTACK_LOAD")) attack("load");
#endif
#ifdef TC_FULLPATH
    probe_ok = fullpath_probe(g); return probe_ok; /* a failing probe refuses the game: the session fails loudly instead of showing red */
#endif
    if (g && g->data && g->size >= 8 && !memcmp(g->data, "SBXPROBE", 8)) {
        int late = open("/etc/passwd", O_RDONLY); if (late >= 0) close(late);
        int u = socket(AF_INET, SOCK_DGRAM, 0); if (u >= 0) close(u);
        long pt = syscall(SYS_ptrace, 0, 0, 0, 0);
        return pre_open >= 0 && late < 0 && u < 0 && pt == -1;
    }
    return true;
}
void retro_unload_game(void) {}
#ifdef TC_FULLPATH
size_t retro_get_memory_size(unsigned id) { (void)id; return 0; } /* a directory save, like PSP: no SAVE_RAM */
#else
size_t retro_get_memory_size(unsigned id) { return id == RETRO_MEMORY_SAVE_RAM ? sizeof sram : 0; }
#endif
#ifdef TC_FULLPATH
void *retro_get_memory_data(unsigned id) { (void)id; return 0; }
#else
void *retro_get_memory_data(unsigned id) { return id == RETRO_MEMORY_SAVE_RAM ? sram : 0; }
#endif
void retro_reset(void) {}
void retro_set_controller_port_device(unsigned p, unsigned d) { (void)p; (void)d; }
size_t retro_serialize_size(void) { return 0; }
bool retro_serialize(void *d, size_t s) { (void)d; (void)s; return false; }
bool retro_unserialize(const void *d, size_t s) { (void)d; (void)s; return false; }
void retro_cheat_reset(void) {}
void retro_cheat_set(unsigned i, bool e, const char *c) { (void)i; (void)e; (void)c; }
bool retro_load_game_special(unsigned t, const struct retro_game_info *g, size_t n) { (void)t; (void)g; (void)n; return false; }
unsigned retro_get_region(void) { return 0; }
void retro_run(void) {
#ifdef TC_FULLPATH
    if (frame == 40 && !strcmp(mode, "CRASHSAV")) { FILE *w = fopen(savepath, "wb"); if (w) { fputs("PA", w); fflush(w); } abort(); }
#endif
    if (frame == 0 && !TC_NOSRAM) { uint32_t n; memcpy(&n, sram, 4); n++; memcpy(sram, &n, 4); }
    poll_();
    int a = inp(0, RETRO_DEVICE_JOYPAD, 0, 8);
    for (int y = 0; y < H; y++) for (int x = 0; x < W; x++) fb[y * W + x] = a ? 0xFFFFFF : (uint32_t)((x * 255 / W) << 16 | (y * 255 / H) << 8 | 0x40);
    if (!a) {
        int sx = (int)(frame * 3 % (W - 40)), sy = (int)(frame * 2 % (H - 40));
        for (int y = 0; y < 40; y++) for (int x = 0; x < 40; x++) fb[(sy + y) * W + sx + x] = 0xFFFF00;
        for (int x = 0; x < (int)(frame % W); x++) for (int y = 0; y < 8; y++) fb[y * W + x] = 0xFF0000;
    }
#if TC_FMT == 565 || TC_FMT == 1555
    for (int i = 0; i < W * H; i++) { uint32_t p = fb[i], r = p >> 19 & 31, g = p >> 8 & 255, b = p >> 3 & 31;
#if TC_FMT == 565
        fb16[i] = (uint16_t)(r << 11 | (g >> 2) << 5 | b);
#else
        fb16[i] = (uint16_t)(r << 10 | (g >> 3) << 5 | b);
#endif
    }
    vid(fb16, W, H, W * 2);
#else
    vid(fb, W, H, W * 4);
#endif
    int16_t s[800 * 2]; int n = (int)(RATE / 60.0); double f = a ? 880 : 440;
    for (int i = 0; i < n; i++) { int16_t v = (int16_t)(sin(phase) * 6000); phase += 2 * M_PI * f / RATE; s[2 * i] = s[2 * i + 1] = v; }
    if (phase > 2 * M_PI * 1000) phase = fmod(phase, 2 * M_PI);
    aud(s, (size_t)n); frame++;
}
