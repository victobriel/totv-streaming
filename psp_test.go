package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A ROM file of `size` bytes that starts with the magic the pspcore test core expects. Sparse: it costs no disk.
func bigFakePSPROM(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "game.iso")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("PSPTEST1")
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func pspEnv(t *testing.T) *testEnv {
	t.Helper()
	rw := t.TempDir()
	e := newEnv(t, func(c *config) { c.RWTmp = rw; c.DirSaveEvery = time.Hour; c.RWAllowDisk = true })
	if iso := e.srv.iso; !iso.Seccomp || !iso.Landlock {
		t.Skip("no seccomp/landlock here")
	}
	return e
}

func (e *testEnv) pspBody(id string) map[string]any {
	b := e.createBody(id)
	b["platform"], b["core"] = "psp", "server_pspcore"
	return b
}

func savedCounter(t *testing.T, z []byte) (map[string]uint32, error) {
	t.Helper()
	ents, err := parseSaveZip(z, pspSavePrefix, pspSaveLimits)
	if err != nil {
		return nil, err
	}
	zr, _ := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	out := map[string]uint32{}
	for i, e := range ents {
		rc, _ := zr.File[i].Open()
		var b [4]byte
		rc.Read(b[:])
		rc.Close()
		out[e.name] = binary.LittleEndian.Uint32(b[:])
	}
	return out, nil
}

// need_fullpath: a 256 MiB ROM is downloaded to <session>/rom (a directory of its own), the host never loads it into memory (the
// child's peak RSS stays tiny), the core opens it by path under a read-only Landlock rule and cannot write next to it, it writes its
// memory stick into the only writable directory, and at the end the parent turns PSP/SAVEDATA into the zip the Totv wants. A second
// session restores that zip (counter 1 -> 2). The shader-cache-like file never travels.
func TestPSPFullPathSessionAndSave(t *testing.T) {
	e := pspEnv(t)
	e.totv.romPath = bigFakePSPROM(t, 256<<20)
	// a file in the HOST's /dev/shm: the core (which builds its RAM in a shm file) must not see it, it gets a private tmpfs
	if err := os.WriteFile("/dev/shm/totv-host-marker", []byte("host"), 0o600); err != nil {
		t.Skipf("cannot write the host /dev/shm marker: %v", err)
	}
	t.Cleanup(func() { os.Remove("/dev/shm/totv-host-marker") })
	if _, m, _ := e.call("GET", "/v1/health", nil, testSecret); m["cores"].(map[string]any)["psp"] == nil {
		t.Fatalf("health does not list the psp test core: %v", m["cores"])
	}
	for round, want := range []uint32{1, 2} {
		id := "psp" + string(rune('a'+round))
		code, m, raw := e.call("POST", "/v1/sessions", e.pspBody(id), testSecret)
		if code != 201 {
			t.Fatalf("round %d: create %d %s (the core's probes failing makes the load fail)", round, code, raw)
		}
		_ = m
		ss := e.srv.sessions[id]
		if ss.flags&3 != 3 {
			t.Fatalf("sandbox flags %d", ss.flags)
		}
		st, _ := os.ReadFile("/proc/" + itoa(ss.corePID()) + "/status")
		var hwm int
		for _, l := range strings.Split(string(st), "\n") {
			if strings.HasPrefix(l, "VmHWM:") {
				hwm = atoiFirst(l)
			}
		}
		if hwm == 0 || hwm > 100*1024 {
			t.Fatalf("child peak RSS %d kB: a 256 MiB ROM must not be in memory", hwm)
		}
		t.Logf("round %d: child VmHWM %d kB with a 256 MiB ROM", round, hwm)
		if fi, err := os.Stat(filepath.Join(ss.dir, "rom", "game.iso")); err != nil || fi.Size() != 256<<20 {
			t.Fatalf("rom not where expected: %v", err)
		}
		ssRW := ss.rwDir
		code, m, raw = e.call("DELETE", "/v1/sessions/"+id, nil, testSecret)
		if code != 200 || m["saved"] != true {
			t.Fatalf("delete: %d %s", code, raw)
		}
		if _, err := os.Stat(ssRW); err == nil {
			t.Fatal("the writable directory outlived the session")
		}
		got, err := savedCounter(t, e.totv.sram)
		if err != nil || len(got) != 1 || got[pspSavePrefix+"TEST00/DATA.BIN"] != want {
			t.Fatalf("round %d: saved package %v err %v, want counter %d and nothing else (no shader cache)", round, got, err, want)
		}
		if e.totv.puts != round+1 || !strings.Contains(e.totv.lastQuery, "core=server_pspcore") {
			t.Fatalf("puts %d query %q", e.totv.puts, e.totv.lastQuery)
		}
	}
}

func TestPSPRefusesHostileSaveAndZipROM(t *testing.T) {
	e := pspEnv(t)
	e.totv.romPath = bigFakePSPROM(t, 1<<20)
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create(pspSavePrefix + "../../../escape.txt")
	w.Write([]byte("x"))
	zw.Close()
	e.totv.sram = b.Bytes()
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("evil"), testSecret); code != 502 || !strings.Contains(string(raw), "save_load_failed") {
		t.Fatalf("hostile save: %d %s", code, raw)
	}
	e.totv.sram = nil
	// a zip as a PSP ROM is refused (rom_invalid), the session never starts
	b.Reset()
	zw = zip.NewWriter(&b)
	w, _ = zw.Create("game.iso")
	w.Write([]byte("PSPTEST1"))
	zw.Close()
	zp := filepath.Join(t.TempDir(), "z.zip")
	os.WriteFile(zp, b.Bytes(), 0o600)
	e.totv.romPath = zp
	if code, _, raw := e.call("POST", "/v1/sessions", e.pspBody("zipped"), testSecret); code != 502 || !strings.Contains(string(raw), "rom_invalid") {
		t.Fatalf("zip rom: %d %s", code, raw)
	}
}

func TestPSPROMExtAndHostOptions(t *testing.T) {
	for in, want := range map[string]string{"CISO\x00\x00": ".cso", "\x00PBP\x00": ".pbp", "\x00\x00\x00\x00\x00": ".iso", "PSPT": ".iso"} {
		if got, err := pspROMExt([]byte(in[:4])); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := pspROMExt([]byte("PK\x03\x04")); err == nil {
		t.Error("zip accepted as a PSP ROM")
	}
	s := &server{cfg: config{}}
	o := s.hostOptions("ppsspp", coreTable["ppsspp"], "/tmp/rw-1", "/opt/cores/system")
	for _, w := range []string{"save=/tmp/rw-1", "ro=/opt/cores/system", "gl=core46", "v.ppsspp_internal_resolution=480x272"} {
		if !strings.Contains(o, w) {
			t.Errorf("host options without %q: %s", w, o)
		}
	}
	// the option string is ';' and '=' separated: a path carrying them must never reach it
	for _, bad := range []string{"/tmp/a;dev=/dev/sda", "/tmp/a=b", "rel/dir", "/tmp/../etc"} {
		if o := s.hostOptions("ppsspp", coreTable["ppsspp"], bad, bad); strings.Contains(o, "save=") || strings.Contains(o, "ro=") {
			t.Errorf("unsafe path %q passed: %s", bad, o)
		}
	}
	if o := pspCoreOptions("ppsspp_x=1;mupen64plus-evil=1;ppsspp_y=2=3;ppsspp_z=a\nb"); strings.Contains(o[len(o)-1], "z") || len(o) != 7 {
		t.Errorf("core options not sanitized: %v", o)
	}
	o2 := strings.Join(pspCoreOptions("ppsspp_internal_resolution=1920x1088"), ";")
	if strings.Contains(o2, "480x272") || !strings.Contains(o2, "v.ppsspp_internal_resolution=1920x1088") {
		t.Errorf("an override must replace the default (the host takes the first match): %s", o2)
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + intStr(n)) }
func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
func atoiFirst(l string) int {
	n := 0
	for _, c := range l {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else if n > 0 {
			break
		}
	}
	return n
}

func TestPSPButtonMapping(t *testing.T) {
	// packet bit -> RetroPad bit -> what PPSSPP's libretro.cpp makes of it
	cases := []struct {
		bit  uint
		want uint16
		name string
	}{
		{4, 1 << 0, "a (south) -> RetroPad B -> Cross"}, {5, 1 << 8, "b (east) -> RetroPad A -> Circle"},
		{6, 1 << 1, "x (west) -> RetroPad Y -> Square"}, {7, 1 << 9, "y (north) -> RetroPad X -> Triangle"},
		{8, 1 << 10, "l1 -> L"}, {10, 1 << 10, "l2 -> L"}, {9, 1 << 11, "r1 -> R"}, {11, 1 << 11, "r2 -> R"},
		{14, 1 << 3, "start"}, {15, 1 << 2, "select"}, {0, 1 << 4, "dpad up"}, {1, 1 << 5, "dpad down"}, {2, 1 << 6, "dpad left"}, {3, 1 << 7, "dpad right"},
		{12, 0, "l3 has no PSP button"}, {13, 0, "r3 has no PSP button"}, {16, 0, "guide is ignored"},
	}
	for _, c := range cases {
		if m := retroMaskFor("psp", 1<<c.bit); m != c.want {
			t.Errorf("%s: got %#x want %#x", c.name, m, c.want)
		}
	}
	if !hasAnalog("psp") || !hasAnalog("n64") || hasAnalog("snes") {
		t.Error("hasAnalog")
	}
}
