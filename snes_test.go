package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// TestRealSNES (SNES=1, `make snes`) loads a real ROM (a .zip from a directory OUTSIDE the repo, read only, never
// copied in) into the real snes9x core through the component, receives the H.264 video with a test client, decodes it
// with ffmpeg, saves PNG captures and proves that a button press changes the game state.
//
//	SNES=1 SNES_ROM=<a ROM or .zip outside the repo> SNES_OUT=/some/dir go test -run TestRealSNES -v
func TestRealSNES(t *testing.T) {
	if os.Getenv("SNES") != "1" {
		t.Skip("set SNES=1 (make snes)")
	}
	romPath := os.Getenv("SNES_ROM")
	if romPath == "" {
		t.Skip("set SNES_ROM=<a SNES ROM or zip outside the repository>")
	}
	cwd, _ := os.Getwd()
	coresDir := filepath.Join(cwd, "cores")
	if _, err := os.Stat(filepath.Join(coresDir, "snes9x_libretro.so")); err != nil {
		t.Skip("cores/snes9x_libretro.so not built")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	rom, err := os.ReadFile(romPath)
	if err != nil {
		t.Skip("ROM not available: ", err)
	}
	out := os.Getenv("SNES_OUT")
	if out == "" {
		out = t.TempDir()
	}
	os.MkdirAll(out, 0o755)

	e := newEnv(t, func(c *config) { c.CoresDir = coresDir; c.DevTestCore = false })
	e.totv.rom = rom
	body := e.createBody("snes")
	body["core"] = "server_snes9x"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()

	const W, H = 640, 480
	ff := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32",
		"-analyzeduration", "0", "-f", "h264", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "-fps_mode", "passthrough", "pipe:1")
	in, _ := ff.StdinPipe()
	fout, _ := ff.StdoutPipe()
	if err := ff.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); ff.Process.Kill(); ff.Wait() }()
	var mu sync.Mutex
	var last []byte
	var nframes int
	go func() {
		for {
			buf := make([]byte, W*H*3)
			if _, err := io.ReadFull(fout, buf); err != nil {
				return
			}
			mu.Lock()
			last, nframes = buf, nframes+1
			mu.Unlock()
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
	p.negotiate(t, e, "snes")
	grab := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), last...)
	}
	save := func(name string, b []byte) {
		img := image.NewRGBA(image.Rect(0, 0, W, H))
		for i := 0; i < W*H; i++ {
			img.Set(i%W, i/W, color.RGBA{b[3*i], b[3*i+1], b[3*i+2], 255})
		}
		var buf bytes.Buffer
		png.Encode(&buf, img)
		if err := os.WriteFile(filepath.Join(out, name), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// distinct colors (quantized) and mean abs diff
	colors := func(b []byte) int {
		m := map[[3]byte]bool{}
		for i := 0; i+2 < len(b); i += 3 {
			m[[3]byte{b[i] >> 4, b[i+1] >> 4, b[i+2] >> 4}] = true
		}
		return len(m)
	}
	diff := func(a, b []byte) float64 {
		s := 0
		for i := range a {
			d := int(a[i]) - int(b[i])
			if d < 0 {
				d = -d
			}
			s += d
		}
		return float64(s) / float64(len(a))
	}
	tap := func(bit uint, hold time.Duration) {
		p.sendInput(1 << bit)
		time.Sleep(hold)
		p.sendInput(0)
	}
	waitFor(t, "decoded video", 15*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return nframes > 5 })

	delay := 9 * time.Second // boot + intro until the title screen
	if v := os.Getenv("SNES_BOOT_SEC"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil {
			delay = d
		}
	}
	for el := time.Duration(0); el < delay; el += time.Second {
		time.Sleep(time.Second)
		if os.Getenv("SNES_TIMELINE") == "1" {
			save("timeline-"+time.Now().Format("150405")+".png", grab())
		}
	}
	title := grab()
	save("snes-frame.png", title)
	nc := colors(title)
	t.Logf("frame before Start: %d distinct colors (4-bit quantized)", nc)
	if nc < 8 {
		t.Fatalf("frame looks blank (%d colors)", nc)
	}
	// Start (packet bit 14), twice with a pause: first may only skip an intro
	tap(14, 150*time.Millisecond)
	for i := 0; i < 6; i++ { // fade out, then the file select screen
		time.Sleep(time.Second)
		if os.Getenv("SNES_TIMELINE") == "1" {
			save("post-"+time.Now().Format("150405")+".png", grab())
		}
	}
	after := grab()
	save("snes-frame-after-start.png", after)
	d := diff(title, after)
	t.Logf("mean abs diff before/after Start: %.2f", d)
	if d < 3 {
		t.Fatalf("Start changed nothing (diff %.2f)", d)
	}
	_, st, _ := e.call("GET", "/v1/sessions/snes", nil, testSecret)
	b, _ := jsonIndent(st["stats"])
	t.Logf("stats: %s", b)
}
