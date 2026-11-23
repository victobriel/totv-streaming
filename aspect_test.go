// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAspectRects: where the host places a picture in the fixed output, for every mode (retro-emu --recttest prints the rectangles).
func TestAspectRects(t *testing.T) {
	emu := "bin/retro-emu"
	for _, c := range []struct {
		mode, pic, out string // pic = "dar w h"
		want           string // dst x0 y0 x1 y1, src x0 y0 x1 y1, nearest
	}{
		{"auto", "1.3333 640 480", "1280 720", "160 0 1120 720 0 0 640 480 0"},    // 4:3 core: pillarbox
		{"auto", "1.7778 960 544", "1280 720", "0 0 1280 720 0 0 960 544 0"},      // 16:9 core: fills (within 3%)
		{"auto", "1.3333 640 480", "640 480", "0 0 640 480 0 0 640 480 0"},        // "safe": the frame is 4:3 already
		{"4:3", "1.7778 960 544", "1280 720", "160 0 1120 720 0 0 960 544 0"},     // forced 4:3 whatever the core says
		{"16:9", "1.3333 640 480", "1280 720", "0 0 1280 720 0 0 640 480 0"},      // stretched over the whole frame
		{"fill", "1.3333 640 480", "1280 720", "0 0 1280 720 0 60 640 420 0"},     // covers the frame: top and bottom cropped, aspect kept
		{"fill", "1.7778 960 544", "1280 720", "0 0 1280 720 0 0 960 544 0"},      // already 16:9: nothing to crop
		{"pixel", "1.3333 640 480", "1280 720", "320 120 960 600 0 0 640 480 1"},  // 1x, centered
		{"pixel", "1.3333 320 240", "1280 720", "160 0 1120 720 0 0 320 240 1"},   // 3x, centered
		{"pixel", "1.3333 1280 896", "1280 720", "160 0 1120 720 0 0 1280 896 0"}, // does not fit once: like auto
	} {
		args := append(append([]string{"--recttest", c.mode}, strings.Fields(c.pic)...), strings.Fields(c.out)...)
		out, err := exec.Command(emu, args...).Output()
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("%s %s in %s: %q, want %q", c.mode, c.pic, c.out, got, c.want)
		}
	}
	if err := exec.Command(emu, "--recttest", "zoom", "1.3", "640", "480", "1280", "720").Run(); err == nil {
		t.Error("an unknown mode was accepted by the host")
	}
}

func (e *testEnv) patchRaw(id, body, secret string) int {
	e.t.Helper()
	req, _ := http.NewRequest("PATCH", "http://unix/v1/sessions/"+id, bytes.NewReader([]byte(body)))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// TestAspectAPI: PATCH /v1/sessions/{id} {"aspect":...} and the optional "aspect" of the create request, validated, authenticated, reported by GET.
func TestAspectAPI(t *testing.T) {
	e := newEnv(t, nil)
	b := e.createBody("as")
	b["aspect"] = "zoom"
	if code, _, _ := e.call("POST", "/v1/sessions", b, testSecret); code != 400 {
		t.Fatalf("invalid aspect at create: %d", code)
	}
	b["aspect"] = "fill"
	if code, m, _ := e.call("POST", "/v1/sessions", b, testSecret); code != 201 {
		t.Fatalf("create with aspect: %d %v", code, m)
	}
	if _, st, _ := e.call("GET", "/v1/sessions/as", nil, testSecret); st["aspect"] != "fill" {
		t.Fatalf("aspect after create: %v", st["aspect"])
	}
	for _, a := range aspectNames {
		code, m, _ := e.call("PATCH", "/v1/sessions/as", map[string]string{"aspect": a}, testSecret)
		if code != 200 || m["ok"] != true || m["aspect"] != a {
			t.Fatalf("patch %s: %d %v", a, code, m)
		}
		if _, st, _ := e.call("GET", "/v1/sessions/as", nil, testSecret); st["aspect"] != a {
			t.Fatalf("GET after patch %s: %v", a, st["aspect"])
		}
	}
	for name, body := range map[string]string{"unknown mode": `{"aspect":"zoom"}`, "empty": ``, "empty object": `{}`, "extra field": `{"aspect":"4:3","quality":"safe"}`,
		"only another field": `{"quality":"safe"}`, "number": `{"aspect":4}`, "null": `{"aspect":null}`, "two values": `{"aspect":"4:3"}{"aspect":"16:9"}`, "not json": `aspect=fill`} {
		if code := e.patchRaw("as", body, testSecret); code != 400 {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code := e.patchRaw("as", `{"aspect":"auto"}`, ""); code != 401 {
		t.Errorf("no bearer: %d", code)
	}
	if code := e.patchRaw("as", `{"aspect":"auto"}`, "wrong"); code != 401 {
		t.Errorf("wrong bearer: %d", code)
	}
	if code := e.patchRaw("nope", `{"aspect":"auto"}`, testSecret); code != 404 {
		t.Errorf("unknown session: %d", code)
	}
	if _, st, _ := e.call("GET", "/v1/sessions/as", nil, testSecret); st["aspect"] != "pixel" {
		t.Errorf("a refused PATCH changed the mode: %v", st["aspect"])
	}
}

// TestGLAspectLive: the GPU path in the real sandbox, a 1280x720 stream (quality high): the glcore picture (4:3, dark blue background) is a pillarbox in
// "auto", "4:3" and "pixel", fills the frame in "16:9" and "fill"; switching in a live session wipes the bars and keeps the same encoder (no restart).
func TestGLAspectLive(t *testing.T) {
	e := newEnv(t, nil)
	skipWithoutGPU(t, e)
	body := e.createBody("ga")
	body["platform"], body["core"], body["quality"] = "n64", "server_glcore", "high"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	d := startDecoder(t, p, 1280, 720)
	p.negotiate(t, e, "ga")
	waitFor(t, "decoded video", 15*time.Second, func() bool { _, n := d.grab(); return n > 10 })
	if _, st, _ := e.call("GET", "/v1/sessions/ga", nil, testSecret); st["output"] != "1280x720" || st["aspect"] != "auto" {
		t.Fatalf("status: %v %v", st["output"], st["aspect"])
	}
	bar := func(b []byte) bool {
		return near(pxAt(b, 1280, 40, 360), 0, 0, 0, 6) && near(pxAt(b, 1280, 1240, 360), 0, 0, 0, 6)
	}
	full := func(b []byte) bool {
		return near(pxAt(b, 1280, 40, 360), 0, 0, 64, 14) && near(pxAt(b, 1280, 1240, 360), 0, 0, 64, 14)
	}
	for _, c := range []struct {
		mode string
		ok   func([]byte) bool
		what string
	}{{"16:9", full, "full frame"}, {"auto", bar, "bars"}, {"fill", full, "full frame"}, {"4:3", bar, "bars"}, {"16:9", full, "full frame"}, {"pixel", bar, "bars"}} {
		if code, _, _ := e.call("PATCH", "/v1/sessions/ga", map[string]string{"aspect": c.mode}, testSecret); code != 200 {
			t.Fatalf("patch %s: %d", c.mode, code)
		}
		waitFor(t, c.mode+": "+c.what, 5*time.Second, func() bool {
			b, _ := d.grab()
			return len(b) > 0 && c.ok(b) && near(pxAt(b, 1280, 640, 4), 0, 0, 64, 14)
		})
	}
}
