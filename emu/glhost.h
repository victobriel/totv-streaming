/* glhost.h: headless OpenGL for HW_RENDER libretro cores (N64 etc.), included by host.c. GPL-3.0-or-later.
 *
 * - libEGL is dlopen()ed (retro-emu still runs on a machine without EGL; only GPU cores need it).
 * - Display: EGL_EXT_platform_device (no X11, no Wayland, no GBM); surfaceless context (pbuffer fallback).
 * - gl_pre_init() runs BEFORE the sandbox: it opens /dev/nvidia (all), /dev/dri (all), loads the driver libraries, creates
 *   the context and warms the driver up (a draw, a shader, a readback), so the descriptors and mappings the driver
 *   needs already exist when seccomp/Landlock are applied. The core only asks for its context in retro_load_game
 *   (after the sandbox): gl_hw_render() accepts it only if it is what was created, otherwise the load fails (no
 *   silent downgrade).
 * - Output: the core renders into a host-owned FBO (get_current_framebuffer); every frame it is blitted on the GPU
 *   (scale + vertical flip) into the output-size FBO and read back with pglReadPixels into a PBO (async by default).
 */
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GL/gl.h>
#include <GL/glext.h>

static struct {
    int ok;                        /* context created and current */
    int rb_sync;                   /* 1: pglReadPixels straight into memory (no PBO), 0: PBO ring (one frame behind) */
    int linear;                    /* blit filter */
    unsigned want_type, want_major, want_minor;
    EGLDisplay dpy; EGLContext ctx; EGLSurface surf;
    char renderer[128], version[64];
    struct retro_hw_render_callback hw;   /* what the core asked for */
    int have_hw;
    GLuint core_fbo, core_tex, core_depth, out_fbo, out_tex, pbo[2], q[4];
    unsigned core_w, core_h; int pbo_i, pbo_primed; int q_i, q_n;
    unsigned char *ro;             /* last frame read back (BGRA, top row first) */
    double gpu_ms_sum; unsigned gpu_ms_n;
} G;

static void *egl_lib;
static void *(*p_eglGetProcAddress)(const char *);
typedef void (*glfn)(void);
#define GLF(ret, name, args) static ret (*name) args;
GLF(void, pglGenFramebuffers, (GLsizei, GLuint *))
GLF(void, pglBindFramebuffer, (GLenum, GLuint))
GLF(void, pglFramebufferTexture2D, (GLenum, GLenum, GLenum, GLuint, GLint))
GLF(void, pglFramebufferRenderbuffer, (GLenum, GLenum, GLenum, GLuint))
GLF(GLenum, pglCheckFramebufferStatus, (GLenum))
GLF(void, pglGenRenderbuffers, (GLsizei, GLuint *))
GLF(void, pglBindRenderbuffer, (GLenum, GLuint))
GLF(void, pglRenderbufferStorage, (GLenum, GLenum, GLsizei, GLsizei))
GLF(void, pglDeleteFramebuffers, (GLsizei, const GLuint *))
GLF(void, pglDeleteRenderbuffers, (GLsizei, const GLuint *))
GLF(void, pglBlitFramebuffer, (GLint, GLint, GLint, GLint, GLint, GLint, GLint, GLint, GLbitfield, GLenum))
GLF(void, pglGenBuffers, (GLsizei, GLuint *))
GLF(void, pglBindBuffer, (GLenum, GLuint))
GLF(void, pglBufferData, (GLenum, GLsizeiptr, const void *, GLenum))
GLF(void *, pglMapBufferRange, (GLenum, GLintptr, GLsizeiptr, GLbitfield))
GLF(GLboolean, pglUnmapBuffer, (GLenum))
GLF(void, pglGenQueries, (GLsizei, GLuint *))
GLF(void, pglBeginQuery, (GLenum, GLuint))
GLF(void, pglEndQuery, (GLenum))
GLF(void, pglGetQueryObjectui64v, (GLuint, GLenum, GLuint64 *))
GLF(const GLubyte *, pglGetString, (GLenum))
GLF(void, pglGenTextures, (GLsizei, GLuint *))
GLF(void, pglBindTexture, (GLenum, GLuint))
GLF(void, pglTexImage2D, (GLenum, GLint, GLint, GLsizei, GLsizei, GLint, GLenum, GLenum, const void *))
GLF(void, pglTexParameteri, (GLenum, GLenum, GLint))
GLF(void, pglReadPixels, (GLint, GLint, GLsizei, GLsizei, GLenum, GLenum, void *))
GLF(void, pglReadBuffer, (GLenum))
GLF(void, pglFinish, (void))
GLF(void, pglGetIntegerv, (GLenum, GLint *))
GLF(GLenum, pglGetError, (void))
GLF(void, pglClearColor, (GLfloat, GLfloat, GLfloat, GLfloat))
GLF(void, pglClear, (GLbitfield))
GLF(void, pglPixelStorei, (GLenum, GLint))
GLF(void, pglGenVertexArrays, (GLsizei, GLuint *))
GLF(void, pglBindVertexArray, (GLuint))
GLF(GLuint, pglCreateShader, (GLenum))
GLF(void, pglShaderSource, (GLuint, GLsizei, const GLchar *const *, const GLint *))
GLF(void, pglCompileShader, (GLuint))
GLF(GLuint, pglCreateProgram, (void))
GLF(void, pglAttachShader, (GLuint, GLuint))
GLF(void, pglLinkProgram, (GLuint))
GLF(void, pglUseProgram, (GLuint))
GLF(void, pglDrawArrays, (GLenum, GLint, GLsizei))
GLF(void, pglViewport, (GLint, GLint, GLsizei, GLsizei))
GLF(void, pglDeleteProgram, (GLuint))
GLF(void, pglDeleteShader, (GLuint))
GLF(void, pglDeleteTextures, (GLsizei, const GLuint *))
GLF(void, pglDeleteVertexArrays, (GLsizei, const GLuint *))
GLF(void, pglDeleteBuffers, (GLsizei, const GLuint *))
GLF(void, pglDeleteQueries, (GLsizei, const GLuint *))

/* EGL entry points are looked up with dlsym (no link-time dependency on libEGL) */
static EGLBoolean (*p_eglInitialize)(EGLDisplay, EGLint *, EGLint *);
static EGLBoolean (*p_eglBindAPI)(EGLenum);
static EGLBoolean (*p_eglChooseConfig)(EGLDisplay, const EGLint *, EGLConfig *, EGLint, EGLint *);
static EGLContext (*p_eglCreateContext)(EGLDisplay, EGLConfig, EGLContext, const EGLint *);
static EGLBoolean (*p_eglMakeCurrent)(EGLDisplay, EGLSurface, EGLSurface, EGLContext);
static EGLSurface (*p_eglCreatePbufferSurface)(EGLDisplay, EGLConfig, const EGLint *);
#define eglInitialize p_eglInitialize
#define eglBindAPI p_eglBindAPI
#define eglChooseConfig p_eglChooseConfig
#define eglCreateContext p_eglCreateContext
#define eglMakeCurrent p_eglMakeCurrent
#define eglCreatePbufferSurface p_eglCreatePbufferSurface

static void *glsym(const char *n) { return p_eglGetProcAddress ? p_eglGetProcAddress(n) : NULL; }
static int gl_load_funcs(void) {
#define L(n) do { *(void **)&n = glsym(#n + 1); if (!n) return 0; } while (0)
    L(pglGenFramebuffers); L(pglBindFramebuffer); L(pglFramebufferTexture2D); L(pglFramebufferRenderbuffer); L(pglCheckFramebufferStatus);
    L(pglGenRenderbuffers); L(pglBindRenderbuffer); L(pglRenderbufferStorage); L(pglDeleteFramebuffers); L(pglDeleteRenderbuffers);
    L(pglBlitFramebuffer); L(pglGenBuffers); L(pglBindBuffer); L(pglBufferData); L(pglMapBufferRange); L(pglUnmapBuffer);
    L(pglGenQueries); L(pglBeginQuery); L(pglEndQuery); L(pglGetQueryObjectui64v); L(pglGetString); L(pglGenTextures); L(pglBindTexture);
    L(pglTexImage2D); L(pglTexParameteri); L(pglReadPixels); L(pglReadBuffer); L(pglFinish); L(pglGetIntegerv); L(pglGetError); L(pglClearColor); L(pglClear); L(pglPixelStorei);
    L(pglGenVertexArrays); L(pglBindVertexArray); L(pglCreateShader); L(pglShaderSource); L(pglCompileShader); L(pglCreateProgram);
    L(pglAttachShader); L(pglLinkProgram); L(pglUseProgram); L(pglDrawArrays); L(pglViewport); L(pglDeleteProgram); L(pglDeleteShader);
    L(pglDeleteTextures); L(pglDeleteVertexArrays); L(pglDeleteBuffers); L(pglDeleteQueries);
#undef L
    return 1;
}

/* Warm-up: FBO + shader + draw + PBO readback, so that lazily opened driver resources exist before the sandbox. Also run again after the host
 * closed the GPU descriptors it does not need (see close_gpu_fds in host.c): it checks the pixel, so a context that no longer works is an error
 * at start, never a silently black stream. */
static const char *gl_warmup(void) {
    pglGetError();
    GLuint fbo, tex, vao, pbo, sh, prog;
    pglGenTextures(1, &tex); pglBindTexture(GL_TEXTURE_2D, tex);
    pglTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA8, 64, 64, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
    pglGenFramebuffers(1, &fbo); pglBindFramebuffer(GL_FRAMEBUFFER, fbo);
    pglFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, tex, 0);
    if (pglCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) return "warm-up FBO incomplete";
    pglGenVertexArrays(1, &vao); pglBindVertexArray(vao);
    static const char *vs = "#version 330 core\nvoid main(){vec2 p=vec2((gl_VertexID<<1)&2,gl_VertexID&2);gl_Position=vec4(p*2.0-1.0,0.0,1.0);}\n";
    static const char *fs = "#version 330 core\nout vec4 c;void main(){c=vec4(1.0,0.5,0.25,1.0);}\n";
    prog = pglCreateProgram();
    sh = pglCreateShader(GL_VERTEX_SHADER); pglShaderSource(sh, 1, &vs, NULL); pglCompileShader(sh); pglAttachShader(prog, sh);
    GLuint sf = pglCreateShader(GL_FRAGMENT_SHADER); pglShaderSource(sf, 1, &fs, NULL); pglCompileShader(sf); pglAttachShader(prog, sf);
    pglLinkProgram(prog); pglUseProgram(prog); pglViewport(0, 0, 64, 64); pglDrawArrays(GL_TRIANGLES, 0, 3);
    pglGenBuffers(1, &pbo); pglBindBuffer(GL_PIXEL_PACK_BUFFER, pbo); pglBufferData(GL_PIXEL_PACK_BUFFER, 64 * 64 * 4, NULL, GL_STREAM_READ);
    pglReadPixels(0, 0, 64, 64, GL_BGRA, GL_UNSIGNED_BYTE, 0);
    unsigned char *m = pglMapBufferRange(GL_PIXEL_PACK_BUFFER, 0, 64 * 64 * 4, GL_MAP_READ_BIT);
    int px_ok = m && m[0] >= 60 && m[0] <= 68 && m[1] >= 124 && m[1] <= 132 && m[2] >= 251 && m[3] == 255; /* BGRA of (1.0, 0.5, 0.25) */
    if (m) pglUnmapBuffer(GL_PIXEL_PACK_BUFFER);
    pglFinish();
    pglBindBuffer(GL_PIXEL_PACK_BUFFER, 0); pglUseProgram(0); pglBindVertexArray(0); pglBindFramebuffer(GL_FRAMEBUFFER, 0);
    pglDeleteBuffers(1, &pbo); pglDeleteProgram(prog); pglDeleteShader(sh); pglDeleteShader(sf); pglDeleteVertexArrays(1, &vao);
    pglDeleteFramebuffers(1, &fbo); pglDeleteTextures(1, &tex);
    if (!px_ok || pglGetError() != 0) return "GL warm-up readback failed";
    return NULL;
}

/* Creates the display and the context the core is going to ask for. Returns NULL or an error text (static). */
static const char *gl_pre_init(unsigned type, unsigned major, unsigned minor) {
    G.want_type = type; G.want_major = major; G.want_minor = minor;
    egl_lib = dlopen("libEGL.so.1", RTLD_NOW | RTLD_GLOBAL);
    if (!egl_lib) return "libEGL.so.1 not found";
    *(void **)&p_eglGetProcAddress = dlsym(egl_lib, "eglGetProcAddress");
    if (!p_eglGetProcAddress) return "eglGetProcAddress missing";
#define E(n) do { *(void **)&p_##n = dlsym(egl_lib, #n); if (!p_##n) return #n " missing"; } while (0)
    E(eglInitialize); E(eglBindAPI); E(eglChooseConfig); E(eglCreateContext); E(eglMakeCurrent); E(eglCreatePbufferSurface);
#undef E
    PFNEGLQUERYDEVICESEXTPROC qdev = (PFNEGLQUERYDEVICESEXTPROC)p_eglGetProcAddress("eglQueryDevicesEXT");
    PFNEGLGETPLATFORMDISPLAYEXTPROC gpd = (PFNEGLGETPLATFORMDISPLAYEXTPROC)p_eglGetProcAddress("eglGetPlatformDisplayEXT");
    if (!qdev || !gpd) return "EGL_EXT_device_enumeration / platform_device missing";
    EGLDeviceEXT devs[16]; EGLint nd = 0;
    if (!qdev(16, devs, &nd) || nd < 1) return "no EGL device";
    /* first device that really is a GPU (not the software one): the NVIDIA/Mesa render nodes show up as DRM devices */
    PFNEGLQUERYDEVICESTRINGEXTPROC qstr = (PFNEGLQUERYDEVICESTRINGEXTPROC)p_eglGetProcAddress("eglQueryDeviceStringEXT");
    EGLDeviceEXT dev = devs[0];
    for (int i = 0; i < nd && qstr; i++) if (qstr(devs[i], EGL_DRM_DEVICE_FILE_EXT)) { dev = devs[i]; break; }
    G.dpy = gpd(EGL_PLATFORM_DEVICE_EXT, dev, NULL);
    if (G.dpy == EGL_NO_DISPLAY) return "eglGetPlatformDisplay failed";
    EGLint maj, mnr;
    if (!eglInitialize(G.dpy, &maj, &mnr)) return "eglInitialize failed";
    int gles = type == RETRO_HW_CONTEXT_OPENGLES2 || type == RETRO_HW_CONTEXT_OPENGLES3 || type == RETRO_HW_CONTEXT_OPENGLES_VERSION;
    if (gles) return "GLES contexts are not wired in this prototype";
    if (!eglBindAPI(EGL_OPENGL_API)) return "eglBindAPI(OPENGL) failed";
    const EGLint ca[] = { EGL_SURFACE_TYPE, EGL_PBUFFER_BIT, EGL_RENDERABLE_TYPE, EGL_OPENGL_BIT, EGL_RED_SIZE, 8, EGL_GREEN_SIZE, 8, EGL_BLUE_SIZE, 8, EGL_ALPHA_SIZE, 8, EGL_NONE };
    EGLConfig cfg; EGLint ncfg = 0;
    if (!eglChooseConfig(G.dpy, ca, &cfg, 1, &ncfg) || ncfg < 1) return "eglChooseConfig failed";
    EGLint cx[12]; int n = 0;
    cx[n++] = EGL_CONTEXT_MAJOR_VERSION; cx[n++] = (EGLint)(major ? major : 3);
    cx[n++] = EGL_CONTEXT_MINOR_VERSION; cx[n++] = (EGLint)minor;
    if (type == RETRO_HW_CONTEXT_OPENGL_CORE) { cx[n++] = EGL_CONTEXT_OPENGL_PROFILE_MASK; cx[n++] = EGL_CONTEXT_OPENGL_CORE_PROFILE_BIT; }
    else { cx[n++] = EGL_CONTEXT_OPENGL_PROFILE_MASK; cx[n++] = EGL_CONTEXT_OPENGL_COMPATIBILITY_PROFILE_BIT; }
    cx[n++] = EGL_NONE;
    G.ctx = eglCreateContext(G.dpy, cfg, EGL_NO_CONTEXT, cx);
    if (G.ctx == EGL_NO_CONTEXT) return "eglCreateContext failed (profile/version)";
    G.surf = EGL_NO_SURFACE;
    if (!eglMakeCurrent(G.dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, G.ctx)) { /* no surfaceless support: 16x16 pbuffer */
        const EGLint pa[] = { EGL_WIDTH, 16, EGL_HEIGHT, 16, EGL_NONE };
        G.surf = eglCreatePbufferSurface(G.dpy, cfg, pa);
        if (G.surf == EGL_NO_SURFACE || !eglMakeCurrent(G.dpy, G.surf, G.surf, G.ctx)) return "eglMakeCurrent failed";
    }
    if (!gl_load_funcs()) return "GL 3.x entry points missing";
    snprintf(G.renderer, sizeof G.renderer, "%s", (const char *)pglGetString(GL_RENDERER));
    snprintf(G.version, sizeof G.version, "%s", (const char *)pglGetString(GL_VERSION));
    const char *we = gl_warmup();
    if (we) return we;
    G.ok = 1;
    return NULL;
}

static uintptr_t gl_get_current_framebuffer(void) { return G.core_fbo; }
static retro_proc_address_t gl_get_proc_address(const char *sym) { return (retro_proc_address_t)glsym(sym); }

/* SET_HW_RENDER from the core: accept only what gl_pre_init() created. */
static bool gl_hw_render(struct retro_hw_render_callback *hw) {
    if (!G.ok) return false;
    if (hw->context_type != G.want_type) return false;
    if (hw->version_major > G.want_major || (hw->version_major == G.want_major && hw->version_minor > G.want_minor)) return false;
    hw->get_current_framebuffer = gl_get_current_framebuffer;
    hw->get_proc_address = gl_get_proc_address;
    G.hw = *hw; G.have_hw = 1;
    return true;
}

/* after retro_load_game: the framebuffers the core renders into (max geometry) and the output-size ones */
static const char *gl_make_targets(unsigned core_w, unsigned core_h, unsigned ow, unsigned oh) {
    G.core_w = core_w; G.core_h = core_h;
    pglGenTextures(1, &G.core_tex); pglBindTexture(GL_TEXTURE_2D, G.core_tex);
    pglTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA8, (GLsizei)core_w, (GLsizei)core_h, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
    pglTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR); pglTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
    pglGenRenderbuffers(1, &G.core_depth); pglBindRenderbuffer(GL_RENDERBUFFER, G.core_depth);
    pglRenderbufferStorage(GL_RENDERBUFFER, GL_DEPTH24_STENCIL8, (GLsizei)core_w, (GLsizei)core_h);
    pglGenFramebuffers(1, &G.core_fbo); pglBindFramebuffer(GL_FRAMEBUFFER, G.core_fbo);
    pglFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, G.core_tex, 0);
    pglFramebufferRenderbuffer(GL_FRAMEBUFFER, GL_DEPTH_STENCIL_ATTACHMENT, GL_RENDERBUFFER, G.core_depth);
    if (pglCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) return "core FBO incomplete";
    pglGenTextures(1, &G.out_tex); pglBindTexture(GL_TEXTURE_2D, G.out_tex);
    pglTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA8, (GLsizei)ow, (GLsizei)oh, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
    pglGenFramebuffers(1, &G.out_fbo); pglBindFramebuffer(GL_FRAMEBUFFER, G.out_fbo);
    pglFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, G.out_tex, 0);
    if (pglCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) return "out FBO incomplete";
    pglGenBuffers(2, G.pbo);
    for (int i = 0; i < 2; i++) { pglBindBuffer(GL_PIXEL_PACK_BUFFER, G.pbo[i]); pglBufferData(GL_PIXEL_PACK_BUFFER, (GLsizeiptr)ow * oh * 4, NULL, GL_STREAM_READ); }
    pglBindBuffer(GL_PIXEL_PACK_BUFFER, 0);
    pglGenQueries(4, G.q);
    G.ro = calloc((size_t)ow * oh, 4);
    pglBindFramebuffer(GL_FRAMEBUFFER, G.core_fbo);
    pglClearColor(0, 0, 0, 1); pglClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT | GL_STENCIL_BUFFER_BIT);
    return NULL;
}

static void gl_frame_begin(void) {
    pglBindFramebuffer(GL_FRAMEBUFFER, G.core_fbo);
    pglBeginQuery(GL_TIME_ELAPSED, G.q[G.q_i]);
}
/* t_blit/t_read (seconds) are measured by the caller around the two phases; returns 1 when G.ro holds a new frame.
 * The query started in gl_frame_begin() spans retro_run() and the blit, and is read two frames later (no stall). */
static int gl_frame_end(unsigned w, unsigned h, unsigned ow, unsigned oh, double *t_blit, double *t_read) {
    struct timespec a, b, c;
    clock_gettime(CLOCK_MONOTONIC, &a);
    if (w > G.core_w) w = G.core_w;
    if (h > G.core_h) h = G.core_h;
    pglBindFramebuffer(GL_READ_FRAMEBUFFER, G.core_fbo); pglBindFramebuffer(GL_DRAW_FRAMEBUFFER, G.out_fbo);
    if (getenv("RETRO_EMU_GLDEBUG")) { /* debugging only (never set by the parent): sample the core FBO and report GL errors */
        static int n; static unsigned char big[1920 * 1080 * 4]; GLint cur = 0; pglGetIntegerv(GL_FRAMEBUFFER_BINDING, &cur);
        pglReadPixels(0, 0, (GLsizei)w, (GLsizei)h, GL_RGBA, GL_UNSIGNED_BYTE, big);
        unsigned nz = 0, mx = 0; for (unsigned i = 0; i < w * h; i++) { if (big[4 * i] | big[4 * i + 1] | big[4 * i + 2]) nz++; if (big[4 * i] > mx) mx = big[4 * i]; }
        if (n++ % 60 == 0) fprintf(stderr, "gldebug: frame %d core_fbo nonblack px %u of %u, max R %u, glerr 0x%x w=%u h=%u\n", n, nz, w * h, mx, pglGetError(), w, h);
    }
    /* bottom_left_origin: row 0 of the FBO is the bottom of the picture; pglReadPixels also starts at the bottom, so flip once here */
    if (G.hw.bottom_left_origin) pglBlitFramebuffer(0, 0, (GLint)w, (GLint)h, 0, (GLint)oh, (GLint)ow, 0, GL_COLOR_BUFFER_BIT, G.linear ? GL_LINEAR : GL_NEAREST);
    else pglBlitFramebuffer(0, 0, (GLint)w, (GLint)h, 0, 0, (GLint)ow, (GLint)oh, GL_COLOR_BUFFER_BIT, G.linear ? GL_LINEAR : GL_NEAREST);
    pglEndQuery(GL_TIME_ELAPSED);
    pglBindFramebuffer(GL_READ_FRAMEBUFFER, G.out_fbo);
    pglReadBuffer(GL_COLOR_ATTACHMENT0);
    clock_gettime(CLOCK_MONOTONIC, &b);
    int got = 0; size_t sz = (size_t)ow * oh * 4;
    if (G.rb_sync) {
        pglReadPixels(0, 0, (GLsizei)ow, (GLsizei)oh, GL_BGRA, GL_UNSIGNED_BYTE, G.ro); /* waits for the whole frame on the GPU */
        got = 1;
    } else {
        pglBindBuffer(GL_PIXEL_PACK_BUFFER, G.pbo[G.pbo_i]);
        pglReadPixels(0, 0, (GLsizei)ow, (GLsizei)oh, GL_BGRA, GL_UNSIGNED_BYTE, 0);   /* async DMA into this PBO */
        if (G.pbo_primed) {                                                           /* map the PBO of the previous frame */
            pglBindBuffer(GL_PIXEL_PACK_BUFFER, G.pbo[G.pbo_i ^ 1]);
            void *m = pglMapBufferRange(GL_PIXEL_PACK_BUFFER, 0, (GLsizeiptr)sz, GL_MAP_READ_BIT);
            if (m) { memcpy(G.ro, m, sz); pglUnmapBuffer(GL_PIXEL_PACK_BUFFER); got = 1; }
        }
        pglBindBuffer(GL_PIXEL_PACK_BUFFER, 0);
        G.pbo_primed = 1; G.pbo_i ^= 1;
    }
    clock_gettime(CLOCK_MONOTONIC, &c);
    /* GPU time of a frame two submissions ago (its result is surely ready) */
    G.q_n++;
    if (G.q_n >= 3) {
        GLuint64 ns = 0; pglGetQueryObjectui64v(G.q[(G.q_i + 2) % 4], GL_QUERY_RESULT, &ns);
        G.gpu_ms_sum += (double)ns / 1e6; G.gpu_ms_n++;
    }
    G.q_i = (G.q_i + 1) % 4;
    pglBindFramebuffer(GL_FRAMEBUFFER, 0);
    *t_blit = (double)(b.tv_sec - a.tv_sec) + (b.tv_nsec - a.tv_nsec) / 1e9;
    *t_read = (double)(c.tv_sec - b.tv_sec) + (c.tv_nsec - b.tv_nsec) / 1e9;
    return got;
}
