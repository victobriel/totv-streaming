// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

// Hardening, round 2: per-session limits (space, quota, file size, tmpfs), the single-thread rule before the sandbox, no save after a crash,
// DELETE during a slow start.

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// pspModeROM: what the pspcore test core takes as a game: "PSPTEST1", an 8 byte misbehaviour name (see emu/testcore/testcore.c), padding.
func pspModeROM(t *testing.T, mode string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "game.iso")
	if err := os.WriteFile(p, append([]byte("PSPTEST1"+mode), make([]byte, 1<<20)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitGone(t *testing.T, e *testEnv, id string, d time.Duration) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if code, _, _ := e.call("GET", "/v1/sessions/"+id, nil, testSecret); code == 404 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// M1: a core (driver) that starts a thread while it loads, before the sandbox, is refused: Landlock restricts one thread only.
func TestHostRefusesThreadsBeforeSandbox(t *testing.T) {
	cwd, _ := os.Getwd()
	rom := filepath.Join(t.TempDir(), "rom.bin")
	_ = os.WriteFile(rom, bytes.Repeat([]byte{1}, 4096), 0o600)
	cmd := exec.Command(testEmuBin(), filepath.Join(cwd, "bin", "threadcore_libretro.so"), rom, "-", t.TempDir(), "640", "480")
	cmd.Env = []string{}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(out, hdr[:]); err != nil || hdr[0] != 'E' {
		t.Fatalf("expected an error message, got %q err %v", hdr[0], err)
	}
	msg := make([]byte, binary.LittleEndian.Uint32(hdr[4:]))
	_, _ = io.ReadFull(out, msg)
	_ = cmd.Wait()
	if !strings.Contains(string(msg), "single-threaded") {
		t.Fatalf("message %q", msg)
	}
	// and the ordinary core, with no thread, still starts (control)
	e := newEnv(t, nil)
	e.create("one-thread")
}

// M2: RLIMIT_FSIZE in the child: the core cannot grow a file past the limit (EFBIG; the test core checks it from inside)
func TestChildFileSizeLimit(t *testing.T) {
	e := pspEnv(t)
	e.srv.cfg.FileMax = 128 << 20
	e.totv.romPath = pspModeROM(t, "FSIZETST")
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("fs"), testSecret); code != 201 {
		t.Fatalf("the file size limit did not hold inside the child (its probe failed the load): %d %s", code, raw)
	}
	e.call("DELETE", "/v1/sessions/fs", nil, testSecret)
}

// M2: the writable directory has a quota: a core that fills it is ended and nothing it wrote is uploaded
func TestRWQuotaEndsSession(t *testing.T) {
	e := pspEnv(t)
	e.srv.cfg.RWMax = 1 << 20
	e.srv.cfg.QuotaEvery = 100 * time.Millisecond
	e.totv.romPath = pspModeROM(t, "FILLRW")
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("fill"), testSecret); code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	rw := e.srv.sessions["fill"].rwDir
	if !waitGone(t, e, "fill", 10*time.Second) {
		t.Fatal("the session was not ended after the core wrote more than the quota")
	}
	if _, err := os.Stat(rw); err == nil {
		t.Error("the writable directory outlived the session")
	}
	if e.totv.puts != 0 {
		t.Errorf("a save was uploaded after the quota was exceeded (%d puts)", e.totv.puts)
	}
}

// BAIXO 9: empty files cost no bytes but fill the inode table: the entry quota ends the session too
func TestRWEntryQuotaEndsSession(t *testing.T) {
	e := pspEnv(t)
	e.srv.cfg.RWFiles = 100
	e.srv.cfg.QuotaEvery = 100 * time.Millisecond
	e.totv.romPath = pspModeROM(t, "FILLINO")
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("inodes"), testSecret); code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	if !waitGone(t, e, "inodes", 10*time.Second) {
		t.Fatal("the session was not ended after the core created more entries than the quota")
	}
	if e.totv.puts != 0 {
		t.Errorf("a save was uploaded after the quota was exceeded (%d puts)", e.totv.puts)
	}
}

// M4: a core that dies in the middle of writing its save does not get that half file packed and uploaded
func TestNoSaveAfterCrash(t *testing.T) {
	e := pspEnv(t)
	e.totv.romPath = pspModeROM(t, "CRASHSAV")
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("crash"), testSecret); code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	if !waitGone(t, e, "crash", 15*time.Second) {
		t.Fatal("the session did not end after the core crashed")
	}
	e.totv.mu.Lock()
	defer e.totv.mu.Unlock()
	if e.totv.puts != 0 || e.totv.sram != nil {
		t.Fatalf("a save was uploaded after a crash: puts=%d sram=%q", e.totv.puts, e.totv.sram)
	}
}

// M2: not enough free space for the ROM (Content-Length + slack): refused before a byte is written
func TestStartRefusedWithoutFreeSpace(t *testing.T) {
	e := newEnv(t, nil)
	old := freeBytes
	freeBytes = func(string) (uint64, error) { return 1 << 20, nil } // 1 MiB free
	defer func() { freeBytes = old }()
	code, _, raw := e.call("POST", "/v1/sessions", e.createBody("nospace"), testSecret)
	if code != 502 || !strings.Contains(string(raw), "no_space") {
		t.Fatalf("expected no_space, got %d %s", code, raw)
	}
	if ents, _ := filepath.Glob(filepath.Join(e.srv.cfg.TmpRoot, "ses-*")); len(ents) != 0 {
		t.Errorf("session directory left behind: %v", ents)
	}
}

// M2: TOTV_STREAM_RW_TMP must be a tmpfs (README says so; the code now checks)
func TestRWTmpMustBeTmpfs(t *testing.T) {
	shm, err := os.MkdirTemp("/tmp", "totv-rw-") // any tmpfs but /dev/shm
	if err != nil {
		t.Skip("no /tmp")
	}
	defer os.RemoveAll(shm)
	if ok, _, _ := tmpfsInfo(shm); !ok {
		t.Skip("/tmp is not a tmpfs here")
	}
	if dev, err := os.MkdirTemp("/dev/shm", "totv-rw-"); err == nil {
		defer os.RemoveAll(dev)
		if (config{RWTmp: dev}).checkRWTmp() == nil {
			t.Error("a directory under /dev/shm was accepted (the core's private /dev/shm hides it)")
		}
	}
	disk := t.TempDir()
	if ok, _, _ := tmpfsInfo(disk); ok {
		t.Skip("TMPDIR is a tmpfs here: run with TMPDIR on a disk")
	}
	if err := (config{RWTmp: disk}).checkRWTmp(); err == nil {
		t.Error("a disk directory was accepted as the writable tmpfs")
	}
	if err := (config{RWTmp: disk, RWAllowDisk: true}).checkRWTmp(); err != nil {
		t.Errorf("the development override was ignored: %v", err)
	}
	if err := (config{RWTmp: shm}).checkRWTmp(); err != nil {
		t.Errorf("a tmpfs was refused: %v", err)
	}
	// through the API: a PSP session is refused with a disk, started with the tmpfs
	e := newEnv(t, func(c *config) { c.RWTmp = disk; c.DirSaveEvery = time.Hour })
	if iso := e.srv.iso; !iso.Seccomp || !iso.Landlock {
		t.Skip("no seccomp/landlock here")
	}
	e.totv.romPath = pspModeROM(t, "")
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("disk"), testSecret); code != 502 || !strings.Contains(string(raw), "rw_tmp_unusable") {
		t.Fatalf("disk rw tmp: %d %s", code, raw)
	}
	e.srv.cfg.RWTmp = shm
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("shm"), testSecret); code != 201 {
		t.Fatalf("tmpfs rw tmp: %d %s", code, raw)
	}
	e.call("DELETE", "/v1/sessions/shm", nil, testSecret)
}

func TestCleanOrphans(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"ses-123", "rw-456", "ses-abc", "other", "ses-"} {
		_ = os.MkdirAll(filepath.Join(root, d, "sub"), 0o700)
		_ = os.WriteFile(filepath.Join(root, d, "sub", "f"), []byte("x"), 0o600)
	}
	target := t.TempDir()
	_ = os.Symlink(target, filepath.Join(root, "ses-777")) // a link named like a session dir: never followed nor removed
	_ = os.WriteFile(filepath.Join(root, "rw-999"), []byte("a file"), 0o600)
	if n := cleanOrphans(root); n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	for d, want := range map[string]bool{"ses-123": false, "rw-456": false, "ses-abc": true, "other": true, "ses-": true, "ses-777": true, "rw-999": true} {
		_, err := os.Lstat(filepath.Join(root, d))
		if (err == nil) != want {
			t.Errorf("%s: exists=%v want %v", d, err == nil, want)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Error("the link target was touched")
	}
}

// A2: DELETE while start() is still downloading a slow ISO returns fast, starts no child and leaks no goroutine
func TestDeleteDuringSlowStart(t *testing.T) {
	e := pspEnv(t)
	e.totv.slowRom = true
	before := goroutinesOf("(*session)")
	var wg sync.WaitGroup
	var postCode int
	var postRaw []byte
	wg.Add(1)
	go func() {
		defer wg.Done()
		postCode, _, postRaw = e.call("POST", "/v1/sessions", e.pspBody("slow"), testSecret)
	}()
	var ss *session
	for i := 0; i < 100 && ss == nil; i++ {
		time.Sleep(50 * time.Millisecond)
		e.srv.mu.Lock()
		ss = e.srv.sessions["slow"]
		e.srv.mu.Unlock()
		if ss != nil {
			ss.pmu.Lock()
			d := ss.dir
			ss.pmu.Unlock()
			if d == "" {
				ss = nil
			}
		}
	}
	if ss == nil {
		t.Fatal("the session never started downloading")
	}
	time.Sleep(300 * time.Millisecond) // inside the download now
	t0 := time.Now()
	code, _, raw := e.call("DELETE", "/v1/sessions/slow", nil, testSecret)
	if d := time.Since(t0); code != 200 || d > 3*time.Second {
		t.Fatalf("DELETE during start: %d %s after %s", code, raw, d)
	}
	wg.Wait()
	if postCode == 201 {
		t.Fatalf("the POST succeeded although the session was deleted: %s", postRaw)
	}
	if ss.process() != nil {
		t.Error("a child was started for a deleted session")
	}
	if ents, _ := filepath.Glob(filepath.Join(e.srv.cfg.TmpRoot, "ses-*")); len(ents) != 0 {
		t.Errorf("session directory left behind: %v", ents)
	}
	// goroutines: none of the session's may remain
	var after string
	for i := 0; i < 50; i++ {
		after = goroutinesOf("(*session)")
		if after == before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after != before {
		t.Errorf("session goroutines leaked after the delete:\n%s", after)
	}
}

// goroutinesOf: the stacks of the goroutines that have `frame` in them (identity of the goroutine is dropped, only its frames compared)
func goroutinesOf(frame string) string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var keep []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) && !strings.Contains(g, "goroutinesOf") {
			if i := strings.Index(g, "\n"); i >= 0 {
				g = g[i+1:]
			}
			keep = append(keep, g)
		}
	}
	return strings.Join(keep, "\n--\n")
}
