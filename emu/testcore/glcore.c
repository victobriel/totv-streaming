/* glcore: libretro core with HW_RENDER (OpenGL core 3.3) and NO ROM: proves the GPU path of the host
 * (EGL context -> core FBO -> blit -> PBO readback -> x264 -> WebRTC). GPL-3.0-or-later.
 * Draws a spinning triangle (red/green/blue corners) on a dark blue background; while RetroPad A is held the background
 * turns white (latency probe, like testcore). 320x240, 440 Hz tone (880 Hz while A). The left analog stick moves the
 * triangle. The first frame's pixels are exact: the corners of the triangle are pure R, G, B (the Go test checks them). */
#define _GNU_SOURCE
#include <math.h>
#include <string.h>
#include <stdio.h>
#include "../libretro.h"
#ifdef GL_HOG
/* hostile variants (build with -DGL_HOG=1: thread bomb, -DGL_HOG=2: memory hog): one second into the game they try to exhaust the per-session cgroup
 * (pids.max / memory.max). Hard caps in the code (2000 threads, 1 GiB) so that a failed limit cannot take the machine down. */
#include <pthread.h>
#include <stdlib.h>
#include <unistd.h>
#if GL_HOG == 1
static void *hog_idle(void *a) { (void)a; for (;;) sleep(1000); return 0; }
#endif
static void hog(void) {
#if GL_HOG == 1
    int n = 0; pthread_t t; while (n < 2000 && !pthread_create(&t, 0, hog_idle, 0)) n++;
    dprintf(2, "glhog: created %d threads\n", n);
#else
    size_t total = 0; while (total < (1u << 30)) { char *p = malloc(16u << 20); if (!p) break; memset(p, 1, 16u << 20); total += 16u << 20; }
    dprintf(2, "glhog: touched %zu MiB\n", total >> 20);
#endif
}
#endif
#define W 320
#define H 240
#define RATE 48000.0
/* GL 3.3 core types/entry points, loaded with get_proc_address (nothing is linked) */
typedef unsigned GLuint; typedef int GLint; typedef unsigned GLenum; typedef int GLsizei; typedef float GLfloat; typedef char GLchar;
#define GL_COLOR_BUFFER_BIT 0x4000
#define GL_TRIANGLES 4
#define GL_VERTEX_SHADER 0x8B31
#define GL_FRAGMENT_SHADER 0x8B30
#define GL_FRAMEBUFFER 0x8D40
static retro_environment_t env; static retro_video_refresh_t vid; static retro_audio_sample_batch_t aud; static retro_input_poll_t poll_; static retro_input_state_t inp;
static struct retro_hw_render_callback hw;
static GLuint prog, vao; static GLint uAng, uOff, uBg; static unsigned frame; static double phase; static int ctx_ok;
static void (*glBindFramebuffer)(GLenum, GLuint); static void (*glViewport)(GLint, GLint, GLsizei, GLsizei);
static void (*glClearColor)(GLfloat, GLfloat, GLfloat, GLfloat); static void (*glClear)(unsigned);
static GLuint (*glCreateShader)(GLenum); static void (*glShaderSource)(GLuint, GLsizei, const GLchar *const *, const GLint *);
static void (*glCompileShader)(GLuint); static GLuint (*glCreateProgram)(void); static void (*glAttachShader)(GLuint, GLuint);
static void (*glLinkProgram)(GLuint); static void (*glUseProgram)(GLuint); static void (*glDrawArrays)(GLenum, GLint, GLsizei);
static void (*glGenVertexArrays)(GLsizei, GLuint *); static void (*glBindVertexArray)(GLuint);
static GLint (*glGetUniformLocation)(GLuint, const GLchar *); static void (*glUniform1f)(GLint, GLfloat); static void (*glUniform2f)(GLint, GLfloat, GLfloat);
static void (*glUniform3f)(GLint, GLfloat, GLfloat, GLfloat);
void retro_set_environment(retro_environment_t e) { env = e; }
void retro_set_video_refresh(retro_video_refresh_t v) { vid = v; }
void retro_set_audio_sample(retro_audio_sample_t a) { (void)a; }
void retro_set_audio_sample_batch(retro_audio_sample_batch_t a) { aud = a; }
void retro_set_input_poll(retro_input_poll_t p) { poll_ = p; }
void retro_set_input_state(retro_input_state_t i) { inp = i; }
void retro_init(void) {}
void retro_deinit(void) {}
unsigned retro_api_version(void) { return RETRO_API_VERSION; }
void retro_get_system_info(struct retro_system_info *i) { memset(i, 0, sizeof *i); i->library_name = "glcore"; i->library_version = "1"; i->valid_extensions = "*"; }
void retro_get_system_av_info(struct retro_system_av_info *i) { memset(i, 0, sizeof *i); i->geometry.base_width = i->geometry.max_width = W; i->geometry.base_height = i->geometry.max_height = H; i->geometry.aspect_ratio = 4.0f / 3; i->timing.fps = 60.0; i->timing.sample_rate = RATE; }
static const char *VS = "#version 330 core\nuniform float ang; uniform vec2 off; out vec3 col;\n"
    "void main(){ vec2 p[3]=vec2[3](vec2(0.0,0.7),vec2(-0.6,-0.5),vec2(0.6,-0.5)); vec3 c[3]=vec3[3](vec3(1,0,0),vec3(0,1,0),vec3(0,0,1));\n"
    "vec2 q=p[gl_VertexID]; float s=sin(ang), k=cos(ang); gl_Position=vec4(vec2(k*q.x-s*q.y, s*q.x+k*q.y)*vec2(0.75,1.0)+off,0.0,1.0); col=c[gl_VertexID]; }\n";
static const char *FS = "#version 330 core\nin vec3 col; uniform vec3 bg; out vec4 o; void main(){ o=vec4(col,1.0); }\n";
#define LD(n) do { *(void **)&n = (void *)hw.get_proc_address(#n); if (!n) return; } while (0)
static void context_reset(void) {
    LD(glBindFramebuffer); LD(glViewport); LD(glClearColor); LD(glClear); LD(glCreateShader); LD(glShaderSource); LD(glCompileShader);
    LD(glCreateProgram); LD(glAttachShader); LD(glLinkProgram); LD(glUseProgram); LD(glDrawArrays); LD(glGenVertexArrays); LD(glBindVertexArray);
    LD(glGetUniformLocation); LD(glUniform1f); LD(glUniform2f); LD(glUniform3f);
    GLuint v = glCreateShader(GL_VERTEX_SHADER); glShaderSource(v, 1, &VS, NULL); glCompileShader(v);
    GLuint f = glCreateShader(GL_FRAGMENT_SHADER); glShaderSource(f, 1, &FS, NULL); glCompileShader(f);
    prog = glCreateProgram(); glAttachShader(prog, v); glAttachShader(prog, f); glLinkProgram(prog);
    glGenVertexArrays(1, &vao);
    uAng = glGetUniformLocation(prog, "ang"); uOff = glGetUniformLocation(prog, "off"); uBg = glGetUniformLocation(prog, "bg");
    ctx_ok = 1;
}
static void context_destroy(void) { ctx_ok = 0; }
bool retro_load_game(const struct retro_game_info *g) {
    (void)g;
    unsigned f = RETRO_PIXEL_FORMAT_XRGB8888; env(RETRO_ENVIRONMENT_SET_PIXEL_FORMAT, &f);
    memset(&hw, 0, sizeof hw);
    hw.context_type = RETRO_HW_CONTEXT_OPENGL_CORE; hw.version_major = 3; hw.version_minor = 3;
    hw.context_reset = context_reset; hw.context_destroy = context_destroy; hw.depth = true; hw.stencil = true; hw.bottom_left_origin = true;
    return env(RETRO_ENVIRONMENT_SET_HW_RENDER, &hw);
}
void retro_unload_game(void) {}
size_t retro_get_memory_size(unsigned id) { (void)id; return 0; }
void *retro_get_memory_data(unsigned id) { (void)id; return 0; }
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
    poll_();
#ifdef GL_HOG
    if (frame == 60) hog();
#endif
    int a = inp(0, RETRO_DEVICE_JOYPAD, 0, 8);
    float lx = inp(0, RETRO_DEVICE_ANALOG, RETRO_DEVICE_INDEX_ANALOG_LEFT, 0) / 32768.0f, ly = inp(0, RETRO_DEVICE_ANALOG, RETRO_DEVICE_INDEX_ANALOG_LEFT, 1) / 32768.0f;
    if (ctx_ok) {
        glBindFramebuffer(GL_FRAMEBUFFER, (GLuint)hw.get_current_framebuffer());
        glViewport(0, 0, W, H);
        if (a) glClearColor(1, 1, 1, 1); else glClearColor(0.0f, 0.0f, 0.25f, 1);
        glClear(GL_COLOR_BUFFER_BIT);
        glUseProgram(prog); glBindVertexArray(vao);
        glUniform1f(uAng, a ? 0.0f : (float)frame * 0.05f); glUniform2f(uOff, lx * 0.5f, -ly * 0.5f); glUniform3f(uBg, 0, 0, 0);
        glDrawArrays(GL_TRIANGLES, 0, 3);
    }
    vid(RETRO_HW_FRAME_BUFFER_VALID, W, H, 0);
    int16_t s[800 * 2]; int n = (int)(RATE / 60.0); double fr = a ? 880 : 440;
    for (int i = 0; i < n; i++) { int16_t v = (int16_t)(sin(phase) * 6000); phase += 2 * M_PI * fr / RATE; s[2 * i] = s[2 * i + 1] = v; }
    if (phase > 2 * M_PI * 1000) phase = fmod(phase, 2 * M_PI);
    aud(s, (size_t)n); frame++;
}
