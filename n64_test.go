package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// decoder pipes the H.264 the peer receives into ffmpeg and keeps the last decoded RGB frame.
type decoder struct {
	mu     sync.Mutex
	last   []byte
	n      int
	w, h   int
	closer func()
}

func startDecoder(t *testing.T, p *testPeer, w, h int) *decoder {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ff := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32",
		"-analyzeduration", "0", "-f", "h264", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "-fps_mode", "passthrough", "pipe:1")
	in, _ := ff.StdinPipe()
	out, _ := ff.StdoutPipe()
	if err := ff.Start(); err != nil {
		t.Fatal(err)
	}
	d := &decoder{w: w, h: h, closer: func() { in.Close(); ff.Process.Kill(); ff.Wait() }}
	go func() {
		for {
			buf := make([]byte, w*h*3)
			if _, err := io.ReadFull(out, buf); err != nil {
				return
			}
			d.mu.Lock()
			d.last, d.n = buf, d.n+1
			d.mu.Unlock()
		}
	}()
	var dep codecs.H264Packet
	var au []byte
	p.onVideo = func(pkt *rtp.Packet) {
		if b, err := dep.Unmarshal(pkt.Payload); err == nil {
			au = append(au, b...)
		}
		if pkt.Marker && len(au) > 0 {
			in.Write(au)
			au = au[:0]
		}
	}
	t.Cleanup(d.closer)
	return d
}

func (d *decoder) grab() ([]byte, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.last...), d.n
}

func (d *decoder) savePNG(t *testing.T, path string, b []byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, d.w, d.h))
	for i := 0; i < d.w*d.h; i++ {
		img.Set(i%d.w, i/d.w, color.RGBA{b[3*i], b[3*i+1], b[3*i+2], 255})
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (p *testPeer) sendInputAxes(buttons uint32, axes [4]int8) {
	p.mu.Lock()
	p.seq++
	b := encodeInput(inputPacket{seq: p.seq, buttons: buttons, axes: axes})
	p.mu.Unlock()
	_ = p.dc.Send(b)
}

func skipWithoutGPU(t *testing.T, e *testEnv) {
	t.Helper()
	if !e.srv.gpu.OK {
		t.Skipf("no EGL/GL on this machine (retro-emu --gltest: %+v)", e.srv.gpu)
	}
}

func pxAt(b []byte, w, x, y int) [3]int {
	i := 3 * (y*w + x)
	return [3]int{int(b[i]), int(b[i+1]), int(b[i+2])}
}

func near(a [3]int, r, g, b, tol int) bool {
	d := func(x, y int) int {
		if x > y {
			return x - y
		}
		return y - x
	}
	return d(a[0], r) <= tol && d(a[1], g) <= tol && d(a[2], b) <= tol
}

// TestGLCoreEndToEnd: no ROM, no commercial software. The test core `glcore` renders a triangle (R, G, B corners on a dark blue
// background) with OpenGL core 3.3 into the framebuffer the host gives it: EGL context (created before the sandbox) ->
// FBO -> blit -> PBO readback -> I420 -> x264 -> WebRTC -> ffmpeg. Proves the GPU path inside the real sandbox:
//   - picture colors are right (no channel swap) and the orientation is right (the red corner is on top at frame 0 only; we check the
//     background and that all three pure corner colors exist);
//   - RetroPad A (packet "b") turns the whole picture white; the left stick (axes 0/1) moves the triangle (analog path);
//   - the child really has seccomp + Landlock (flags 3) AND the GPU flag (16), every thread has Seccomp: 2.
func TestGLCoreEndToEnd(t *testing.T) {
	e := newEnv(t, nil)
	skipWithoutGPU(t, e)
	body := e.createBody("gl")
	body["platform"], body["core"] = "n64", "server_glcore"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	d := startDecoder(t, p, 640, 480)
	p.negotiate(t, e, "gl")
	waitFor(t, "decoded video", 15*time.Second, func() bool { _, n := d.grab(); return n > 10 })

	e.srv.mu.Lock()
	ss := e.srv.sessions["gl"]
	e.srv.mu.Unlock()
	if ss.flags&3 != 3 || ss.flags&16 == 0 {
		t.Fatalf("sandbox flags %d: want seccomp|landlock|gpu", ss.flags)
	}
	// every thread of the child is under the filter; report the thread list for the README
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/status", ss.corePID()))
	for _, tf := range tasks {
		b, _ := os.ReadFile(tf)
		if err := checkProcStatus(string(b)); err != nil {
			t.Errorf("%s: %v", tf, err)
		}
	}
	t.Logf("child threads under seccomp 2: %d", len(tasks))
	// the descriptor surface (GPU isolation, option 2): with the NVIDIA driver only /dev/nvidiactl of all the GPU nodes stays open
	fds := fdTargets(ss.corePID())
	t.Logf("child fds after the sandbox: %v", fds)
	if strings.Contains(e.srv.gpu.Version, "NVIDIA") {
		if ss.flags&256 == 0 {
			t.Errorf("flags %d: the host did not report closing the GPU descriptors", ss.flags)
		}
		for _, f := range fds {
			if forbiddenGPUFD(f) || strings.HasPrefix(f, "/dev/dri/") {
				t.Errorf("the emulator still holds %s", f)
			}
		}
		if n := countFD(fds, "/dev/nvidiactl"); n == 0 {
			t.Errorf("no /dev/nvidiactl left: the context cannot work, yet it rendered?")
		}
	}
	if e.srv.iso.PIDNS && (ss.flags&flagPIDNS == 0 || !childInOwnPIDNS(ss.cmd.Process.Pid)) {
		t.Errorf("no PID namespace (flags %d)", ss.flags)
	}

	// colors: sample the picture for 3 s; the background (0,0,64) must exist and R, G and B corners must all be present somewhere
	var sawBG, sawR, sawG, sawB bool
	for i := 0; i < 30; i++ {
		b, _ := d.grab()
		if len(b) == 0 {
			continue
		}
		for _, pt := range [][2]int{{5, 5}, {630, 470}, {5, 470}, {630, 5}} {
			sawBG = sawBG || near(pxAt(b, 640, pt[0], pt[1]), 0, 0, 64, 14)
		}
		for y := 0; y < 480; y += 3 {
			for x := 0; x < 640; x += 3 {
				px := pxAt(b, 640, x, y)
				sawR = sawR || near(px, 255, 0, 0, 40)
				sawG = sawG || near(px, 0, 255, 0, 40)
				sawB = sawB || near(px, 0, 0, 255, 40)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawBG || !sawR || !sawG || !sawB {
		b, _ := d.grab()
		out := os.Getenv("N64_OUT")
		if out == "" {
			out = t.TempDir()
		}
		d.savePNG(t, filepath.Join(out, "glcore-fail.png"), b)
		t.Fatalf("colors wrong: background=%v R=%v G=%v B=%v (picture saved in %s)", sawBG, sawR, sawG, sawB, out)
	}
	if out := os.Getenv("N64_OUT"); out != "" {
		b, _ := d.grab()
		os.MkdirAll(out, 0o755)
		d.savePNG(t, filepath.Join(out, "glcore-frame.png"), b)
	}

	// RetroPad A (packet "b", bit 5) -> white picture
	p.sendInput(1 << 5)
	waitFor(t, "white picture after A", 3*time.Second, func() bool {
		b, _ := d.grab()
		return len(b) > 0 && near(pxAt(b, 640, 320, 400), 255, 255, 255, 20) && near(pxAt(b, 640, 5, 5), 255, 255, 255, 20)
	})
	p.sendInput(0)
	// analog: triangle centroid moves right when left X = +127 (the triangle is static while A is held; release first)
	waitFor(t, "triangle back", 3*time.Second, func() bool { b, _ := d.grab(); return len(b) > 0 && near(pxAt(b, 640, 5, 5), 0, 0, 64, 14) })
	centroid := func() float64 {
		b, _ := d.grab()
		var sx, n float64
		for y := 0; y < 480; y += 2 {
			for x := 0; x < 640; x += 2 {
				px := pxAt(b, 640, x, y)
				if px[0] > 60 || px[1] > 60 || px[2] > 120 { // anything that is not the dark blue background
					sx += float64(x)
					n++
				}
			}
		}
		if n == 0 {
			return -1
		}
		return sx / n
	}
	c0 := centroid()
	p.sendInputAxes(0, [4]int8{127, 0, 0, 0})
	time.Sleep(800 * time.Millisecond)
	c1 := centroid()
	p.sendInputAxes(0, [4]int8{-128, 0, 0, 0})
	time.Sleep(800 * time.Millisecond)
	c2 := centroid()
	p.sendInputAxes(0, [4]int8{})
	t.Logf("triangle centroid x: neutral %.0f, stick right %.0f, stick left %.0f", c0, c1, c2)
	if !(c1 > c0+60 && c2 < c0-60) {
		t.Fatalf("left stick did not move the triangle (%.0f %.0f %.0f)", c0, c1, c2)
	}
	_, st, _ := e.call("GET", "/v1/sessions/gl", nil, testSecret)
	b, _ := jsonIndent(st["stats"])
	t.Logf("stats: %s", b)
}

// TestGLCoreRefusedWithoutGPUDevices: the Landlock exception list is configuration, not magic. With a core that never needs a lazily opened
// node the session still works (glcore needs none: its GL use is covered by the warm-up), so this test only checks that bogus entries in
// TOTV_STREAM_GPU_DEVICES are not passed to the host.
func TestHostOptionsSanitized(t *testing.T) {
	s := &server{cfg: config{GPUDevices: "/dev/nvidiactl, /etc/passwd;rm=1 ,/dev/dri/renderD128,/dev/a=b,/dev/../etc/shadow,/dev/dri/../../etc/x,/dev//nvidia0,/dev/dri/./x"}}
	o := s.hostOptions("glcore", coreTable["glcore"], "", "")
	for _, want := range []string{"gl=core46", "dev=/dev/nvidiactl", "dev=/dev/dri/renderD128"} {
		if !strings.Contains(o, want) {
			t.Errorf("%q missing in %q", want, o)
		}
	}
	for _, bad := range []string{"passwd", "rm=1", "a=b", "..", "shadow", "nvidia0", "/./"} {
		if strings.Contains(o, bad) {
			t.Errorf("%q leaked into %q", bad, o)
		}
	}
	if got := s.hostOptions("snes9x", coreTable["snes9x"], "", ""); got != "fsize=536870912" { // only the file size limit
		t.Errorf("software core must get no host options but fsize: %q", got)
	}
}

func TestN64ButtonMapping(t *testing.T) {
	// packet bit -> RetroPad bit of the core's "independent C buttons" map
	cases := []struct {
		bit  uint
		want uint16
		name string
	}{
		{4, 1 << 0, "a (south) -> RetroPad B -> N64 A"}, {6, 1 << 1, "x (west) -> RetroPad Y -> N64 B"},
		{5, 1 << 8, "b (east) -> RetroPad A -> C-Down"}, {7, 1 << 9, "y (north) -> RetroPad X -> C-Up"},
		{10, 1 << 12, "l2 -> Z"}, {9, 1 << 13, "r1 -> R"}, {11, 1 << 13, "r2 -> R"}, {8, 1 << 2, "l1 -> L"},
		{12, 1 << 10, "l3 -> C-Left"}, {13, 1 << 11, "r3 -> C-Right"}, {14, 1 << 3, "start"}, {0, 1 << 4, "dpad up"}, {3, 1 << 7, "dpad right"},
	}
	for _, c := range cases {
		if m := retroMaskFor("n64", 1<<c.bit); m != c.want {
			t.Errorf("%s: got %#x want %#x", c.name, m, c.want)
		}
	}
	if retroMaskFor("snes", 1<<4) != 1 || retroMaskFor("n64", 1<<16) != 0 {
		t.Error("SNES table changed or guide is not ignored")
	}
	// the analog stick of the packet is a full int8: -128..127 -> x256 = -32768..32512 (libretro int16, the core scales to its +-80)
	p := inputPacket{axes: [4]int8{127, -128, 0, 1}}
	q, ok := parseInput(encodeInput(p))
	if !ok || q.axes != p.axes {
		t.Fatalf("axes round trip: %v", q.axes)
	}
}

// ---- real N64 (N64=1): a commercial ROM from OUTSIDE the repo, read-only, served from memory by the fake Totv ----

func readProcCPU(pid int) float64 { // seconds of CPU (user+sys, all threads)
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseFloat(f[11], 64)
	k, _ := strconv.ParseFloat(f[12], 64)
	return (u + k) / 100
}
func procField(pid int, key string) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func gpuSample(pid int) (util, memMB float64, procSM string) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=utilization.gpu,memory.used", "--format=csv,noheader,nounits").Output()
	if err == nil {
		fmt.Sscanf(strings.ReplaceAll(string(out), ",", " "), "%f %f", &util, &memMB)
	}
	if o, err := exec.Command("nvidia-smi", "pmon", "-c", "1", "-s", "um").Output(); err == nil {
		for _, l := range strings.Split(string(o), "\n") {
			f := strings.Fields(l)
			if len(f) > 4 && f[1] == strconv.Itoa(pid) {
				procSM = "sm=" + f[3] + "% fb=" + f[9] + "MB"
			}
		}
	}
	return
}

func percentile(v []float64, f float64) float64 {
	if len(v) == 0 {
		return -1
	}
	sort.Float64s(v)
	return v[int(f*float64(len(v)-1))]
}

// TestRealN64 (N64=1, `make n64`): mupen64plus-next + GLideN64 on the GPU, a commercial ROM from outside the repo. Env: N64_ROM, N64_SECS (30),
// N64_RES (native|2x), N64_READBACK (sync|pbo), N64_OUT (PNG dir). Presses Start every 3 s from the title screen.
func TestRealN64(t *testing.T) {
	if os.Getenv("N64") != "1" {
		t.Skip("set N64=1 (make n64)")
	}
	romPath := os.Getenv("N64_ROM")
	if romPath == "" {
		t.Skip("set N64_ROM=<an N64 ROM outside the repository>")
	}
	cwd, _ := os.Getwd()
	coresDir := filepath.Join(cwd, "cores")
	if _, err := os.Stat(filepath.Join(coresDir, "mupen64plus_next_libretro.so")); err != nil {
		t.Skip("cores/mupen64plus_next_libretro.so not built")
	}
	rom, err := os.ReadFile(romPath)
	if err != nil {
		t.Skip("ROM not available: ", err)
	}
	secs := 30
	if v, err := strconv.Atoi(os.Getenv("N64_SECS")); err == nil && v > 0 {
		secs = v
	}
	out := os.Getenv("N64_OUT")
	if out == "" {
		out = t.TempDir()
	}
	os.MkdirAll(out, 0o755)
	e := newEnv(t, func(c *config) {
		c.CoresDir, c.DevTestCore = coresDir, false
		c.N64Internal2x = os.Getenv("N64_RES") == "2x"
		c.ReadbackPBO = os.Getenv("N64_READBACK") == "pbo"
		c.N64CoreOpts = os.Getenv("N64_CORE_OPTS")
	})
	skipWithoutGPU(t, e)
	e.totv.rom = rom
	body := e.createBody("n64")
	body["platform"], body["core"] = "n64", "server_mupen64plus_next"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	d := startDecoder(t, p, 640, 480)
	p.negotiate(t, e, "n64")
	waitFor(t, "decoded video", 20*time.Second, func() bool { _, n := d.grab(); return n > 5 })
	e.srv.mu.Lock()
	ss := e.srv.sessions["n64"]
	e.srv.mu.Unlock()
	pid := ss.corePID()
	t.Logf("sandbox flags %d (1 seccomp, 2 landlock, 4 landlock net, 8 netns, 16 gpu); child pid %d threads %s; /proc status Seccomp=%s NoNewPrivs=%s",
		ss.flags, pid, procField(pid, "Threads"), procField(pid, "Seccomp"), procField(pid, "NoNewPrivs"))
	if ss.flags&3 != 3 {
		t.Fatalf("sandbox flags %d", ss.flags)
	}

	// 30 s of play: Start every 3 s (leaves the title), a stick wiggle, sampling once a second
	type snap struct{ fIn, fEnc, aud float64 }
	get := func() snap {
		_, st, _ := e.call("GET", "/v1/sessions/n64", nil, testSecret)
		s := st["stats"].(map[string]any)
		return snap{s["framesIn"].(float64), s["framesEncoded"].(float64), s["audioPackets"].(float64)}
	}
	time.Sleep(2 * time.Second) // warm-up, not counted
	s0, c0, w0, pc0 := get(), readProcCPU(pid), time.Now(), readProcCPU(os.Getpid())
	var gpuUtil, gpuMem []float64
	var rss []float64
	var maxLum float64
	var perSec []float64
	prev := s0
	shot := map[int]bool{4: true, 10: true, 18: true, 28: true, 40: true, 55: true}
	for i := 1; i <= secs; i++ {
		time.Sleep(time.Second)
		if i%3 == 0 {
			p.sendInput(1 << 14)
			time.Sleep(120 * time.Millisecond)
			p.sendInput(0)
		}
		p.sendInputAxes(0, [4]int8{int8(60 * ((i%2)*2 - 1)), 0, 0, 0})
		cur := get()
		perSec = append(perSec, cur.fIn-prev.fIn)
		prev = cur
		u, m, sm := gpuSample(pid)
		gpuUtil, gpuMem = append(gpuUtil, u), append(gpuMem, m)
		kb, _ := strconv.ParseFloat(strings.Fields(procField(pid, "VmRSS") + " 0")[0], 64)
		rss = append(rss, kb/1024)
		if i%2 == 0 { // brightness of the picture every 2 s: a game scene can be black for a moment (fades), a dead path is black always
			b, _ := d.grab()
			var l float64
			for k := 0; k < len(b); k += 30 {
				l += float64(b[k])
			}
			if l /= float64(len(b) / 30); l > maxLum {
				maxLum = l
			}
		}
		if shot[i] {
			b, _ := d.grab()
			d.savePNG(t, filepath.Join(out, fmt.Sprintf("n64-t%02d.png", i)), b)
			t.Logf("BBOX t=%d %v", i, contentBBox(b, d.w, d.h, 24))
		}
		if i == 5 {
			t.Logf("t=5s %s", sm)
		}
	}
	s1, c1, w1, pc1 := get(), readProcCPU(pid), time.Now(), readProcCPU(os.Getpid())
	el := w1.Sub(w0).Seconds()
	t.Logf("RESULT frames from core in %.1fs: %.0f = %.2f fps sustained; encoded %.0f (%.2f fps); audio RTP packets %.0f (%.1f/s, 50 expected)", el, s1.fIn-s0.fIn, (s1.fIn-s0.fIn)/el, s1.fEnc-s0.fEnc, (s1.fEnc-s0.fEnc)/el, s1.aud-s0.aud, (s1.aud-s0.aud)/el)
	t.Logf("RESULT frames per second (1 s buckets): min %.0f p50 %.0f max %.0f", percentile(append([]float64(nil), perSec...), 0), percentile(append([]float64(nil), perSec...), .5), percentile(append([]float64(nil), perSec...), 1))
	t.Logf("RESULT retro-emu child CPU %.0f%% of one core (core+GL driver+readback, all threads); component+test client process %.0f%% of one core", (c1-c0)/el*100, (pc1-pc0)/el*100)
	t.Logf("RESULT child RSS %.0f..%.0f MB; whole-GPU util p50 %.0f%% max %.0f%% (the machine's GPU is shared with the desktop); GPU mem used %.0f MB", percentile(append([]float64(nil), rss...), 0), percentile(append([]float64(nil), rss...), 1), percentile(append([]float64(nil), gpuUtil...), .5), percentile(append([]float64(nil), gpuUtil...), 1), percentile(append([]float64(nil), gpuMem...), .5))
	if h := ss.hostMsV.Load(); h != nil {
		t.Logf("RESULT host stages (last 120-frame window, avg): %s", h)
	}
	t.Logf("RESULT x264 encode %+v | frame->wire %+v | input->frame %+v | dropped %d", ss.encodeMs.pct(), ss.wireMs.pct(), ss.inputToFrame.pct(), ss.vDropped.Load())
	if got := (s1.fIn - s0.fIn) / el; got < 50 {
		t.Errorf("sustained %.1f fps < 50", got)
	}
	b, _ := d.grab()
	d.savePNG(t, filepath.Join(out, "n64-frame.png"), b)
	// the picture must not be blank: at some point of the run it was clearly lit
	if maxLum < 20 {
		t.Errorf("picture stayed black (max mean luma %.1f)", maxLum)
	}
}

// TestRealN64Save (N64=1): the core exposes ONE save buffer (EEPROM 2 KiB + 4 controller paks 32 KiB + SRAM 32 KiB + FlashRAM 128 KiB =
// 296960 bytes). It must reach the component through the same 'S' path as the SNES SRAM: the size is announced in the hello, a save
// the Totv already has is loaded into the live buffer before the first frame, and it comes back byte for byte when the session ends.
func TestRealN64Save(t *testing.T) {
	if os.Getenv("N64") != "1" {
		t.Skip("set N64=1 (make n64)")
	}
	romPath := os.Getenv("N64_ROM")
	if romPath == "" {
		t.Skip("set N64_ROM=<an N64 ROM outside the repository>")
	}
	cwd, _ := os.Getwd()
	coresDir := filepath.Join(cwd, "cores")
	rom, err := os.ReadFile(romPath)
	if err != nil {
		t.Skip("ROM not available: ", err)
	}
	e := newEnv(t, func(c *config) { c.CoresDir, c.DevTestCore = coresDir, false })
	skipWithoutGPU(t, e)
	e.totv.rom = rom
	// a pattern in the controller pak 2 area (never touched: the core only formats the pak that is plugged in, pak 1, and a commercial game
	// writes its EEPROM checksum at boot, so those regions legitimately change)
	save := make([]byte, 296960)
	for i := 0; i < 256; i++ {
		save[0x800+0x8000+i] = byte(i)
	}
	e.totv.sram = save
	body := e.createBody("n64s")
	body["platform"], body["core"] = "n64", "server_mupen64plus_next"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	e.srv.mu.Lock()
	ss := e.srv.sessions["n64s"]
	e.srv.mu.Unlock()
	if got := ss.sramSize.Load(); got != 296960 {
		t.Fatalf("save buffer announced by the core: %d, want 296960", got)
	}
	time.Sleep(4 * time.Second)
	code, m, _ := e.call("DELETE", "/v1/sessions/n64s", nil, testSecret)
	if code != 200 || m["ok"] != true {
		t.Fatalf("delete: %d %v", code, m)
	}
	e.totv.mu.Lock()
	got, puts := append([]byte(nil), e.totv.sram...), e.totv.puts
	e.totv.mu.Unlock()
	if len(got) != 296960 || !bytes.Equal(got[0x800+0x8000:0x800+0x8000+256], save[0x800+0x8000:0x800+0x8000+256]) {
		var diffs []string
		for i := range got {
			if i < len(save) && got[i] != save[i] && len(diffs) < 8 {
				diffs = append(diffs, fmt.Sprintf("%#x:%02x->%02x", i, save[i], got[i]))
			}
		}
		t.Fatalf("save did not survive the session (len %d, puts %d, first diffs %v)", len(got), puts, diffs)
	}
	t.Logf("RESULT save: %d bytes announced, loaded into the core and returned intact; the core formatted pak 1 and the game wrote its EEPROM header, so the save was uploaded (PUTs: %d)", len(got), puts)
}

// contentBBox is the bounding box [x0,y0,x1,y1) of the pixels whose max channel exceeds thr (RGB24 picture); empty -> zero rect.
func contentBBox(b []byte, w, h, thr int) [4]int {
	x0, y0, x1, y1 := w, h, 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := 3 * (y*w + x)
			if int(b[i]) > thr || int(b[i+1]) > thr || int(b[i+2]) > thr {
				x0, y0, x1, y1 = min(x0, x), min(y0, y), max(x1, x+1), max(y1, y+1)
			}
		}
	}
	if x1 == 0 {
		return [4]int{}
	}
	return [4]int{x0, y0, x1, y1}
}

// fdTargets lists where every descriptor of pid points (readlink of /proc/<pid>/fd/*), sorted.
func fdTargets(pid int) []string {
	ents, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	var out []string
	for _, e := range ents {
		if t, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, e.Name())); err == nil {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func countFD(fds []string, target string) int {
	n := 0
	for _, f := range fds {
		if f == target {
			n++
		}
	}
	return n
}
