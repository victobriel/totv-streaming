// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "s3cret-for-tests"

func TestMain(m *testing.M) {
	// the C parts are built with make; no commercial ROM is needed anywhere (the test core takes any file)
	if out, err := exec.Command("make", "emu").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// fakeTotv stands in for the Totv: serves a "ROM" (any bytes), keeps the SRAM it receives, records headers.
type fakeTotv struct {
	*httptest.Server
	mu         sync.Mutex
	sram       []byte
	puts       int
	badAuth    int
	redirectIt bool
	lastQuery  string
	romPath    string // a big ROM served from a file (PSP test: hundreds of MB, never read into memory)
	slowRom    bool   // the ROM download starts (headers + a few bytes) and then stalls until the client gives up
	rom        []byte // served instead of the dummy ROM when set (real-core test; read from outside the repo, never stored)
}

func newFakeTotv(token string) *fakeTotv {
	f := &fakeTotv{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			f.badAuth++
			http.Error(w, "no", 401)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/base/api/games/rom/"):
			if f.slowRom {
				f.mu.Unlock()
				w.Header().Set("Content-Length", "1048576")
				w.WriteHeader(200)
				w.Write(make([]byte, 1024))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			if f.redirectIt {
				http.Redirect(w, r, "http://127.0.0.1:1/evil", 302)
				return
			}
			if f.romPath != "" {
				http.ServeFile(w, r, f.romPath)
				return
			}
			if f.rom != nil {
				w.Write(f.rom)
				return
			}
			w.Write(bytes.Repeat([]byte{0x42}, 4096))
		case r.URL.Path == "/base/api/games/7/sram" && r.Method == "GET":
			if f.sram == nil {
				http.NotFound(w, r)
				return
			}
			w.Write(f.sram)
		case r.URL.Path == "/base/api/games/7/sram" && r.Method == "PUT":
			f.sram, _ = io.ReadAll(r.Body)
			f.puts++
			f.lastQuery = r.URL.RawQuery
			w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return f
}
func (f *fakeTotv) base() string { return f.URL + "/base" }

type testEnv struct {
	t      *testing.T
	srv    *server
	client *http.Client
	totv   *fakeTotv
}

func newEnv(t *testing.T, mut func(*config)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "retro-stream.secret")
	os.WriteFile(secret, []byte(testSecret+"\n"), 0o600)
	cwd, _ := os.Getwd()
	cfg := config{SockPath: filepath.Join(dir, "control.sock"), SecretFile: secret, CoresDir: filepath.Join(cwd, "bin"),
		EmuBin: filepath.Join(cwd, "bin", "retro-emu"), TmpRoot: dir, UDPPort: 0, DevTestCore: true, MaxSessions: 1, GPUDevices: "/dev/nvidiactl",
		W: 640, H: 480, Kbps: 4000, NoSignalTimeout: 60 * time.Second, DisconnectGrace: 30 * time.Second}
	if m := os.Getenv("TEST_CGROUP"); m != "" { // TEST_CGROUP=require: every session of the run (real cores too) goes through the per-session cgroup
		cfg.CgroupMode = m
	}
	if mut != nil {
		mut(&cfg)
	}
	srv := newServer(cfg, probeIsolation(cfg.EmuBin))
	srv.gpu = cachedGPU(cfg.EmuBin)
	ln, err := listenUnix(cfg.SockPath)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.handler()}
	go hs.Serve(ln)
	e := &testEnv{t: t, srv: srv, totv: newFakeTotv("tok-abc")}
	e.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.SockPath)
	}}, Timeout: 30 * time.Second}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		srv.stopAll(ctx)
		hs.Close()
		e.totv.Close()
	})
	return e
}

func (e *testEnv) call(method, path string, body any, secret string) (int, map[string]any, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://unix"+path, rd)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return res.StatusCode, m, raw
}

func (e *testEnv) createBody(id string) map[string]any {
	return map[string]any{"sessionId": id, "itemId": 7, "mediaId": 9, "platform": "snes", "core": "server_testcore",
		"token": "tok-abc", "serverUrl": e.totv.base(), "iceHost": "127.0.0.1"}
}

func (e *testEnv) create(id string) {
	e.t.Helper()
	if code, m, _ := e.call("POST", "/v1/sessions", e.createBody(id), testSecret); code != 201 || m["id"] != id {
		e.t.Fatalf("create: %d %v", code, m)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
func mustJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", " ") }

var (
	gpuOnce sync.Once
	gpuRes  gpuInfo
)

// cachedGPU runs `retro-emu --gltest` once per test binary (it creates a real GL context, ~0.3 s).
func cachedGPU(emu string) gpuInfo {
	gpuOnce.Do(func() { gpuRes = probeGPU(emu) })
	return gpuRes
}
