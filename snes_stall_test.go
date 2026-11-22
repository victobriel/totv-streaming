package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestSNESKeepsStreaming (SNES=1) runs the real snes9x with a peer that never reads decoded video and sends input
// continuously (as a browser does), and requires the frame counters to keep advancing for 10 s.
func TestSNESKeepsStreaming(t *testing.T) {
	if os.Getenv("SNES") != "1" {
		t.Skip("set SNES=1")
	}
	romPath := os.Getenv("SNES_ROM")
	if romPath == "" {
		t.Skip("set SNES_ROM=<a SNES ROM or zip outside the repository>")
	}
	cwd, _ := os.Getwd()
	coresDir := filepath.Join(cwd, "cores")
	if _, err := os.Stat(filepath.Join(coresDir, "snes9x_libretro.so")); err != nil {
		t.Skip("snes9x not built")
	}
	rom, err := os.ReadFile(romPath)
	if err != nil {
		t.Skip("ROM not available: ", err)
	}
	e := newEnv(t, func(c *config) { c.CoresDir = coresDir; c.DevTestCore = false })
	e.totv.rom = rom
	body := e.createBody("snes")
	body["core"] = "server_snes9x"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	p.negotiate(t, e, "snes")
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(16 * time.Millisecond)
		defer tk.Stop()
		n := 0
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				n++
				p.sendInput(uint32(1) << uint(n/20%12))
			}
		}
	}()
	get := func() (fin, enc, aud float64) {
		_, st, _ := e.call("GET", "/v1/sessions/snes", nil, testSecret)
		s := st["stats"].(map[string]any)
		return s["framesIn"].(float64), s["framesEncoded"].(float64), s["audioPackets"].(float64)
	}
	time.Sleep(2 * time.Second)
	f0, e0, a0 := get()
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		f, en, a := get()
		t.Logf("t=%ds framesIn=%v encoded=%v audio=%v", i+1, f, en, a)
	}
	f1, e1, a1 := get()
	if f1-f0 < 300 || e1-e0 < 300 || a1-a0 < 300 {
		t.Fatalf("stream stalled: framesIn %v->%v encoded %v->%v audio %v->%v", f0, f1, e0, e1, a0, a1)
	}
}

// A child that stays alive but stops producing video (bad ROM, hung core) must not leave a frozen "playing"
// session: the watchdog ends it. SIGSTOP stands in for the stuck core.
func TestStalledEmulatorEndsSession(t *testing.T) {
	e := newEnv(t, func(c *config) { c.NoVideoTimeout = 2 * time.Second })
	e.create("stall")
	waitFor(t, "frames", 10*time.Second, func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		ss := e.srv.sessions["stall"]
		return ss != nil && ss.vFrames.Load() > 5
	})
	e.srv.mu.Lock()
	ss := e.srv.sessions["stall"]
	e.srv.mu.Unlock()
	if err := syscall.Kill(ss.corePID(), syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(ss.corePID(), syscall.SIGCONT)
	waitFor(t, "session ended by the video watchdog", 8*time.Second, func() bool {
		code, _, _ := e.call("GET", "/v1/sessions/stall", nil, testSecret)
		return code == 404
	})
}
