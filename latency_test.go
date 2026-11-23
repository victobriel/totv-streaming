// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// TestLatencyProbe (PROBE=1) measures input -> decoded white frame on THIS machine, over loopback, with the real
// pipeline (testcore -> I420 -> x264 -> RTP/WebRTC -> ffmpeg software decoder). Not glass-to-glass: no display,
// no Wi-Fi, no Fire TV decoder. See README.
func TestLatencyProbe(t *testing.T) {
	if os.Getenv("PROBE") != "1" {
		t.Skip("set PROBE=1 (make latency)")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	e := newEnv(t, nil)
	e.create("lat")
	latencyProbe(t, e, "lat", "testcore (software, no GPU)")
}

// TestLatencyProbeGL (PROBE=1): same measurement through the GPU path (glcore: EGL -> FBO -> blit -> readback -> x264).
func TestLatencyProbeGL(t *testing.T) {
	if os.Getenv("PROBE") != "1" {
		t.Skip("set PROBE=1 (make latency)")
	}
	e := newEnv(t, func(c *config) { c.ReadbackPBO = os.Getenv("TOTV_STREAM_READBACK") == "pbo" })
	skipWithoutGPU(t, e)
	body := e.createBody("latgl")
	body["platform"], body["core"] = "n64", "server_glcore"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	latencyProbe(t, e, "latgl", "glcore (OpenGL core 3.3 on the GPU, readback "+map[bool]string{false: "sync", true: "PBO"}[e.srv.cfg.ReadbackPBO]+")")
}

func latencyProbe(t *testing.T, e *testEnv, id, label string) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	p := newTestPeer(t)
	defer p.pc.Close()

	ff := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32",
		"-analyzeduration", "0", "-threads", "1", "-f", "h264", "-i", "pipe:0", "-vf", "scale=16:12:flags=neighbor,format=gray",
		"-f", "rawvideo", "-fps_mode", "passthrough", "pipe:1")
	in, _ := ff.StdinPipe()
	out, _ := ff.StdoutPipe()
	if err := ff.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); ff.Process.Kill(); ff.Wait() }()

	var mu sync.Mutex
	var mean float64
	var stamp time.Time
	go func() {
		buf := make([]byte, 192)
		for {
			if _, err := io.ReadFull(out, buf); err != nil {
				return
			}
			s := 0
			for _, b := range buf {
				s += int(b)
			}
			mu.Lock()
			mean, stamp = float64(s)/192, time.Now()
			mu.Unlock()
		}
	}()
	var dep codecs.H264Packet
	var au []byte
	p.onVideo = func(pkt *rtp.Packet) {
		b, err := dep.Unmarshal(pkt.Payload)
		if err == nil {
			au = append(au, b...)
		}
		if pkt.Marker && len(au) > 0 {
			in.Write(au)
			au = au[:0]
		}
	}
	p.negotiate(t, e, id)
	wait := func(cond func(float64) bool, d time.Duration) (time.Time, bool) {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			mu.Lock()
			m, s := mean, stamp
			mu.Unlock()
			if !s.IsZero() && cond(m) {
				return s, true
			}
			time.Sleep(500 * time.Microsecond)
		}
		return time.Time{}, false
	}
	if _, ok := wait(func(m float64) bool { return m > 5 }, 8*time.Second); !ok {
		t.Fatal("no decoded video")
	}
	var press, release []float64
	for i := 0; i < 30; i++ {
		wait(func(m float64) bool { return m < 190 }, 2*time.Second)
		time.Sleep(400 * time.Millisecond)
		t0 := p.sendInput(1 << 5) // packet "b" (east) = RetroPad A, which the test core answers with a white frame
		if _, ok := wait(func(m float64) bool { return m > 200 }, 2*time.Second); ok {
			press = append(press, float64(time.Since(t0).Microseconds())/1000)
		}
		time.Sleep(300 * time.Millisecond)
		t1 := p.sendInput(0)
		if _, ok := wait(func(m float64) bool { return m < 190 }, 2*time.Second); ok {
			release = append(release, float64(time.Since(t1).Microseconds())/1000)
		}
	}
	q := func(v []float64, f float64) float64 {
		if len(v) == 0 {
			return -1
		}
		sort.Float64s(v)
		return v[int(f*float64(len(v)-1))]
	}
	fmt.Printf("PROBE [%s] input->decoded frame (loopback, x264 ultrafast, ffmpeg sw decode): press n=%d p50=%.1f ms p95=%.1f ms max=%.1f | release n=%d p50=%.1f ms p95=%.1f ms\n",
		label, len(press), q(press, 0.5), q(press, 0.95), q(press, 1), len(release), q(release, 0.5), q(release, 0.95))
	_, m, _ := e.call("GET", "/v1/sessions/"+id, nil, testSecret)
	b, _ := jsonIndent(m["stats"])
	fmt.Printf("PROBE server stats: %s\n", b)
}
