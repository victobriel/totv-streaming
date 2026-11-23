// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- PS2 without a BIOS or an image: everything here uses a SYNTHETIC BIOS (a valid ROMDIR table around a "b ." loop, written by this file) and the
// ps2core test core. No console BIOS and no game is needed, none is in the repository. ----

// syntheticPS2BIOS is a 4 MiB image with the ROMDIR layout the core checks (RESET, ROMDIR, ROMVER entries) and a MIPS "b ." loop as reset code. It
// is not a console BIOS: it carries nothing of one.
func syntheticPS2BIOS() []byte {
	img := make([]byte, 4<<20)
	binary.LittleEndian.PutUint32(img[0:], 0x1000ffff) // beq zero,zero,-1
	const resetSize = 0x400
	ent := func(n string, sz uint32) []byte {
		e := make([]byte, 16)
		copy(e, n)
		binary.LittleEndian.PutUint32(e[12:], sz)
		return e
	}
	tab := append(ent("RESET", resetSize), ent("ROMDIR", 64)...)
	tab = append(tab, ent("ROMVER", 16)...)
	copy(img[resetSize:], tab)
	copy(img[resetSize+64:], "0250EC20040623")
	return img
}

func writeBIOSDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "bios")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(d, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestPS2BiosScan(t *testing.T) {
	good := syntheticPS2BIOS()
	ps1 := make([]byte, 512<<10) // the size of a PS1 BIOS: too small, and no ROMDIR
	copy(ps1, "not a ps2 rom")
	noVer := syntheticPS2BIOS()
	copy(noVer[0x400+32:], make([]byte, 16)) // ROMVER entry erased: the table now ends there
	noReset := syntheticPS2BIOS()
	copy(noReset[0x400:], make([]byte, 16))
	badVer := syntheticPS2BIOS()
	copy(badVer[0x400+64:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14})
	tiny := syntheticPS2BIOS()[:2<<10]
	cases := []struct {
		name   string
		dir    func() string
		ok     bool
		reason string
		files  int
		valid  int
	}{
		{"no directory configured", func() string { return "" }, false, "bios_missing", 0, 0},
		{"directory does not exist", func() string { return filepath.Join(t.TempDir(), "nope") }, false, "bios_missing", 0, 0},
		{"empty directory", func() string { return writeBIOSDir(t, nil) }, false, "bios_missing", 0, 0},
		{"only companions", func() string { return writeBIOSDir(t, map[string][]byte{"scph50004.nvm": {1}, "x.mec": {1}}) }, false, "bios_missing", 0, 0},
		{"a PS1 BIOS", func() string { return writeBIOSDir(t, map[string][]byte{"SCPH1001.BIN": ps1}) }, false, "bios_invalid", 1, 0},
		{"no ROMVER", func() string { return writeBIOSDir(t, map[string][]byte{"a.bin": noVer}) }, false, "bios_invalid", 1, 0},
		{"no RESET", func() string { return writeBIOSDir(t, map[string][]byte{"a.bin": noReset}) }, false, "bios_invalid", 1, 0},
		{"ROMVER not text", func() string { return writeBIOSDir(t, map[string][]byte{"a.bin": badVer}) }, false, "bios_invalid", 1, 0},
		{"too small", func() string { return writeBIOSDir(t, map[string][]byte{"a.bin": tiny}) }, false, "bios_invalid", 1, 0},
		{"a valid one", func() string { return writeBIOSDir(t, map[string][]byte{"scph50004.bin": good}) }, true, "", 1, 1},
		{"invalid first, valid second", func() string { return writeBIOSDir(t, map[string][]byte{"a.bin": ps1, "b.bin": good}) }, true, "", 2, 1},
		{"a symlink is not followed", func() string {
			d := writeBIOSDir(t, nil)
			real := filepath.Join(t.TempDir(), "real.bin")
			os.WriteFile(real, good, 0o644)
			os.Symlink(real, filepath.Join(d, "link.bin"))
			return d
		}, false, "bios_missing", 0, 0},
	}
	for _, c := range cases {
		r := ps2BiosScan(c.dir())
		if r.OK != c.ok || r.Reason != c.reason || r.Files != c.files || r.Valid != c.valid {
			t.Errorf("%s: got %+v, want ok=%v reason=%q files=%d valid=%d", c.name, r, c.ok, c.reason, c.files, c.valid)
		}
	}
	r := ps2BiosScan(writeBIOSDir(t, map[string][]byte{"scph50004.bin": good, "scph50004.bin.rom1": {1}, "scph50004.erom": {1}, "scph50004.nvm": {1}, "other.rom2": {1}}))
	if !r.OK || r.Name != "scph50004.bin" || r.Version != "0250EC20040623" || len(r.Extras) != 3 {
		t.Errorf("companions: %+v", r)
	}
	if !ps2BiosNameOK("scph50004.bin") || ps2BiosNameOK("../x.bin") || ps2BiosNameOK("a;b") || ps2BiosNameOK("a=b") || ps2BiosNameOK("") || ps2BiosNameOK("a/b") {
		t.Error("ps2BiosNameOK")
	}
}

// ps2Env: a component with the ps2core test core, a (synthetic) BIOS directory, big images on a DISK directory (the test machine's /tmp is a RAM tmpfs).
func ps2Env(t *testing.T, biosDir string, mut func(*config)) *testEnv {
	t.Helper()
	rw := t.TempDir()
	big := diskTmp(t)
	e := newEnv(t, func(c *config) {
		c.RWTmp, c.DirSaveEvery, c.RWAllowDisk = rw, time.Hour, true
		c.PS2BiosDir, c.BigTmp = biosDir, big
		if mut != nil {
			mut(c)
		}
	})
	if iso := e.srv.iso; !iso.Seccomp || !iso.Landlock {
		t.Skip("no seccomp/landlock here")
	}
	return e
}

// diskTmp is a directory on the user's disk (not /tmp, which is RAM here), removed at the end.
func diskTmp(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	root := filepath.Join(base, "totv-streaming-test")
	_ = os.MkdirAll(root, 0o700)
	d, err := os.MkdirTemp(root, "ps2-")
	if err != nil {
		t.Skipf("no disk directory for big images: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func fakePS2ISO(t *testing.T, size int64, mode string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "game.iso")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("PS2TEST1" + (mode + "        ")[:8])
	if err := f.Truncate(size); err != nil { // sparse: the tail is read by the core
		t.Fatal(err)
	}
	f.Seek(size-8, 0)
	f.WriteString("TAILMARK")
	f.Close()
	return p
}

func (e *testEnv) ps2Body(id string) map[string]any {
	b := e.createBody(id)
	b["platform"], b["core"] = "ps2", "server_ps2core"
	return b
}

func TestPS2HealthAndRefusals(t *testing.T) {
	// no BIOS directory configured: ps2.bios=false, the core is not offered, the session is refused with bios_missing before anything is downloaded
	e := ps2Env(t, "", nil)
	e.totv.romPath = fakePS2ISO(t, 1<<20, "")
	_, m, raw := e.call("GET", "/v1/health", nil, testSecret)
	ps2, _ := m["ps2"].(map[string]any)
	if ps2 == nil || ps2["bios"] != false || ps2["reason"] != "bios_missing" || ps2["bios_dir_set"] != false {
		t.Fatalf("health ps2 without a BIOS: %s", raw)
	}
	if m["cores"].(map[string]any)["ps2"] != nil {
		t.Fatalf("the ps2 core is offered without a BIOS: %v", m["cores"])
	}
	if strings.Contains(string(raw), "SCPH") || strings.Contains(string(raw), "scph") {
		t.Fatal("health leaks a BIOS file name")
	}
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("nobios"), testSecret); code != 502 || !strings.Contains(string(raw), "bios_missing") {
		t.Fatalf("no BIOS: %d %s", code, raw)
	}
	if ents, _ := os.ReadDir(e.srv.cfg.BigTmp); len(ents) != 0 {
		t.Fatalf("a refused session left %d entries in the big tmp", len(ents))
	}
	// a PS1 BIOS (the wrong console): bios_invalid
	e.srv.cfg.PS2BiosDir = writeBIOSDir(t, map[string][]byte{"SCPH1001.BIN": make([]byte, 512<<10)})
	_, m, _ = e.call("GET", "/v1/health", nil, testSecret)
	if ps2 = m["ps2"].(map[string]any); ps2["bios"] != false || ps2["reason"] != "bios_invalid" || ps2["bios_files"] != float64(1) || ps2["bios_valid"] != float64(0) {
		t.Fatalf("health with a PS1 BIOS: %v", ps2)
	}
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("ps1"), testSecret); code != 502 || !strings.Contains(string(raw), "bios_invalid") {
		t.Fatalf("PS1 BIOS: %d %s", code, raw)
	}
	// a valid one: health true, the core is offered
	e.srv.cfg.PS2BiosDir = writeBIOSDir(t, map[string][]byte{"scph50004.bin": syntheticPS2BIOS()})
	_, m, _ = e.call("GET", "/v1/health", nil, testSecret)
	if ps2 = m["ps2"].(map[string]any); ps2["bios"] != true || ps2["reason"] != nil || ps2["max_iso_mb"] != float64(9216) {
		t.Fatalf("health with a valid BIOS: %v", ps2)
	}
	if m["cores"].(map[string]any)["ps2"] == nil {
		t.Fatalf("ps2 core not offered with a BIOS: %v", m["cores"])
	}
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write(c)
	}
	zw.Close()
	return b.Bytes()
}

func ps2Cards(t *testing.T, z []byte) map[string][]byte {
	t.Helper()
	ents, err := parseSaveZip(z, ps2SavePrefix, ps2SaveLimits)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	out := map[string][]byte{}
	for i, e := range ents {
		rc, _ := zr.File[i].Open()
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		out[e.name] = b.Bytes()
	}
	return out
}

// The whole path with the PS2 test core: a BIOS from a directory the core can never read, copied per session (the core writes .nvm next to it), an
// image by path that lives on the disk directory (not in the session's RAM), the cards (Mcd001/Mcd002) zipped at the end and restored by the next
// session. PS2_BIG_GB=5 makes the image 5 GiB (beyond the 4 GiB that were the host's old limit; writes 5 GB to disk).
func TestPS2FullPathBIOSAndCards(t *testing.T) {
	size := int64(1536 << 20)
	if gb, err := strconv.Atoi(os.Getenv("PS2_BIG_GB")); err == nil && gb > 0 {
		size = int64(gb) << 30
	}
	bios := writeBIOSDir(t, map[string][]byte{"scph50004.bin": syntheticPS2BIOS(), "scph50004.bin.rom1": []byte("r1"), "ignored.txt": []byte("x")})
	before := dirListing(t, bios)
	e := ps2Env(t, bios, nil)
	e.totv.romPath = fakePS2ISO(t, size, "")
	if err := os.WriteFile("/dev/shm/totv-host-marker", []byte("host"), 0o600); err != nil {
		t.Skipf("cannot write the host /dev/shm marker: %v", err)
	}
	t.Cleanup(func() { os.Remove("/dev/shm/totv-host-marker") })
	for round, want := range []uint32{1, 2} {
		id := "ps2" + string(rune('a'+round))
		t0 := time.Now()
		code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body(id), testSecret)
		if code != 201 {
			t.Fatalf("round %d: create %d %s", round, code, raw)
		}
		t.Logf("round %d: session up in %.1f s with a %d MiB image", round, time.Since(t0).Seconds(), size>>20)
		ss := e.srv.sessions[id]
		if ss.flags&(1|2|4|8|32) != 1|2|4|8|32 {
			t.Fatalf("sandbox flags %d", ss.flags)
		}
		if !strings.HasPrefix(ss.dir, e.srv.cfg.BigTmp) {
			t.Fatalf("the session directory %s is not in the big tmp %s", ss.dir, e.srv.cfg.BigTmp)
		}
		if fi, err := os.Stat(filepath.Join(ss.dir, "rom", "game.iso")); err != nil || fi.Size() != size {
			t.Fatalf("image not where expected: %v", err)
		}
		st, _ := os.ReadFile("/proc/" + itoa(ss.corePID()) + "/status")
		var hwm int
		for _, l := range strings.Split(string(st), "\n") {
			if strings.HasPrefix(l, "VmHWM:") {
				hwm = atoiFirst(l)
			}
		}
		if hwm == 0 || hwm > 100*1024 {
			t.Fatalf("child peak RSS %d kB: the image must not be in memory", hwm)
		}
		t.Logf("round %d: child VmHWM %d kB with the image", round, hwm)
		// the BIOS: a COPY in the session's writable directory (with its companion), the original untouched and never offered to the core
		cp := filepath.Join(ss.rwDir, "pcsx2", "bios")
		if b, err := os.ReadFile(filepath.Join(cp, "scph50004.bin")); err != nil || !bytes.Equal(b, syntheticPS2BIOS()) {
			t.Fatalf("BIOS copy: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(cp, "scph50004.bin.rom1")); err != nil || string(b) != "r1" {
			t.Fatalf("BIOS companion: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cp, "ignored.txt")); err == nil {
			t.Fatal("a file that is not a BIOS companion was copied")
		}
		if _, err := os.Stat(filepath.Join(cp, "scph50004.bin.nvm")); err != nil { // the core wrote it next to its copy
			t.Fatalf("the core could not write next to the BIOS copy: %v", err)
		}
		ssRW := ss.rwDir
		code, m, raw := e.call("DELETE", "/v1/sessions/"+id, nil, testSecret)
		if code != 200 || m["saved"] != true {
			t.Fatalf("delete: %d %s", code, raw)
		}
		if _, err := os.Stat(ssRW); err == nil {
			t.Fatal("the writable directory outlived the session")
		}
		if _, err := os.Stat(ss.dir); err == nil {
			t.Fatal("the session directory (the image) outlived the session")
		}
		cards := ps2Cards(t, e.totv.sram)
		if len(cards) != 2 || len(cards[ps2SavePrefix+"Mcd001.ps2"]) != 8650752 || len(cards[ps2SavePrefix+"Mcd002.ps2"]) != 8650752 {
			t.Fatalf("round %d: package holds %d entries, want exactly the two 8650752-byte cards (no BIOS, no .nvm, no shader cache)", round, len(cards))
		}
		if got := binary.LittleEndian.Uint32(cards[ps2SavePrefix+"Mcd001.ps2"]); got != want {
			t.Fatalf("round %d: card counter %d, want %d (the second session must restore the first one's card)", round, got, want)
		}
		if !strings.Contains(e.totv.lastQuery, "core=server_ps2core") {
			t.Fatalf("query %q", e.totv.lastQuery)
		}
	}
	if after := dirListing(t, bios); after != before {
		t.Fatalf("the operator's BIOS directory changed:\nbefore %s\nafter  %s", before, after)
	}
}

func dirListing(t *testing.T, d string) string {
	t.Helper()
	ents, _ := os.ReadDir(d)
	var b strings.Builder
	for _, e := range ents {
		fi, _ := e.Info()
		b.WriteString(e.Name() + ":" + strconv.FormatInt(fi.Size(), 10) + ":" + fi.ModTime().String() + " ")
	}
	return b.String()
}

func TestPS2RefusesHostileSaveAndImage(t *testing.T) {
	e := ps2Env(t, writeBIOSDir(t, map[string][]byte{"scph50004.bin": syntheticPS2BIOS()}), nil)
	e.totv.romPath = fakePS2ISO(t, 1<<20, "")
	card := make([]byte, 8650752)
	binary.LittleEndian.PutUint32(card, 41)
	// a name that leaves the directory refuses the whole package
	e.totv.sram = zipOf(t, map[string][]byte{ps2SavePrefix + "../../escape": []byte("x")})
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("evil"), testSecret); code != 502 || !strings.Contains(string(raw), "save_load_failed") {
		t.Fatalf("hostile save: %d %s", code, raw)
	}
	// names that are merely not ours are ignored, never extracted: another card file, a BIOS planted under pcsx2/bios, a file outside the prefix
	e.totv.sram = zipOf(t, map[string][]byte{ps2SavePrefix + "Mcd001.ps2": card, ps2SavePrefix + "Mcd009.ps2": []byte("x"), "pcsx2/bios/evil.bin": []byte("x"), "elsewhere": []byte("x")})
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("okz"), testSecret); code != 201 {
		t.Fatalf("a package with extra names must still load: %d %s", code, raw)
	}
	ss := e.srv.sessions["okz"]
	for _, n := range []string{"pcsx2/memcards/Mcd009.ps2", "pcsx2/bios/evil.bin", "elsewhere"} {
		if _, err := os.Stat(filepath.Join(ss.rwDir, n)); err == nil {
			t.Errorf("%s was extracted from the package", n)
		}
	}
	if b, err := os.ReadFile(filepath.Join(ss.rwDir, "pcsx2", "memcards", "Mcd001.ps2")); err != nil || binary.LittleEndian.Uint32(b) != 42 {
		t.Errorf("card not restored (counter %v): %v", b[:4], err)
	}
	e.call("DELETE", "/v1/sessions/okz", nil, testSecret)
	// an image that is a zip is refused
	e.totv.sram = nil
	zp := filepath.Join(t.TempDir(), "z.zip")
	os.WriteFile(zp, zipOf(t, map[string][]byte{"game.iso": []byte("PS2TEST1")}), 0o600)
	e.totv.romPath = zp
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("zipped"), testSecret); code != 502 || !strings.Contains(string(raw), "rom_invalid") {
		t.Fatalf("zip image: %d %s", code, raw)
	}
	// an image over the ceiling is refused before it is written
	e.srv.cfg.PS2MaxISOMB = 1
	e.totv.romPath = fakePS2ISO(t, 4<<20, "")
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("big"), testSecret); code != 502 || !strings.Contains(string(raw), "rom_download_failed") {
		t.Fatalf("image over the ceiling: %d %s", code, raw)
	}
	e.srv.cfg.PS2MaxISOMB = 0
	// not enough free space on the big tmp: no_space before a byte is written
	old := freeBytes
	freeBytes = func(string) (uint64, error) { return 1 << 20, nil }
	code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("full"), testSecret)
	freeBytes = old
	if code != 502 || !strings.Contains(string(raw), "no_space") {
		t.Fatalf("no space: %d %s", code, raw)
	}
	if ents, _ := os.ReadDir(e.srv.cfg.BigTmp); len(ents) != 0 {
		t.Fatalf("refused sessions left %d entries in the big tmp", len(ents))
	}
}

// the quota of the writable directory counts the BIOS copy (4 MiB) and the cards; a core that fills it is ended and nothing it wrote is uploaded
func TestPS2QuotaEndsAFillingCore(t *testing.T) {
	e := ps2Env(t, writeBIOSDir(t, map[string][]byte{"scph50004.bin": syntheticPS2BIOS()}), func(c *config) {
		c.QuotaEvery, c.RWMax = 200*time.Millisecond, 23<<20 // cards 2 x 8650752 + BIOS 4 MiB = 21.5 MiB (sizes, not blocks); the filler adds 3 MiB
	})
	e.totv.romPath = fakePS2ISO(t, 1<<20, "FILLRW")
	if code, _, raw := e.call("POST", "/v1/sessions", e.ps2Body("fill"), testSecret); code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	waitFor(t, "the session over its quota to end", 10*time.Second, func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		_, ok := e.srv.sessions["fill"]
		return !ok
	})
	if e.totv.puts != 0 {
		t.Fatalf("a session killed for its quota uploaded a save (%d puts)", e.totv.puts)
	}
}

func TestPS2ROMExtAndHostOptions(t *testing.T) {
	for in, want := range map[string]string{"MComprHD": ".chd", "CISO\x00\x00\x00\x00": ".cso", "ZISO\x00\x00\x00\x00": ".zso", "\x00\x00\x00\x00\x00\x00\x00\x00": ".iso", "PS2TEST1": ".iso"} {
		if got, err := ps2ROMExt([]byte(in)); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"PK\x03\x04", "\x1f\x8b\x08\x00", "ab"} {
		if _, err := ps2ROMExt([]byte(bad)); err == nil {
			t.Errorf("%q accepted as a PS2 image", bad)
		}
	}
	s := &server{cfg: config{PS2CoreOpts: "pcsx2_upscale_multiplier=2x;pcsx2_bios=evil.bin;pcsx2_shared_memory_cards=disabled;mupen64plus-evil=1;pcsx2_x=1=2;pcsx2_y=a\nb"}}
	o := s.hostOptionsBIOS("pcsx2", coreTable["pcsx2"], "/tmp/rw-1", "/opt/cores/system", "scph50004.bin")
	for _, w := range []string{"save=/tmp/rw-1", "shm=1", "shmmb=1024", "gl=core46", "v.pcsx2_renderer=OpenGL", "v.pcsx2_bios=scph50004.bin", "v.pcsx2_upscale_multiplier=2x",
		"v.pcsx2_shared_memory_cards=enabled", "v.pcsx2_analog_mode1=enabled", "fsize="} {
		if !strings.Contains(o, w) {
			t.Errorf("host options without %q: %s", w, o)
		}
	}
	for _, w := range []string{"evil.bin", "1x (Native)", "mupen", "pcsx2_x", "pcsx2_y", "ro="} {
		if strings.Contains(o, w) {
			t.Errorf("host options carry %q: %s", w, o)
		}
	}
	// a BIOS name that could escape the option string or the directory never reaches it
	for _, bad := range []string{"a;b.bin", "a=b.bin", "../x.bin", "a/b.bin"} {
		if o := s.hostOptionsBIOS("pcsx2", coreTable["pcsx2"], "/tmp/rw-1", "", bad); strings.Contains(o, "pcsx2_bios") {
			t.Errorf("unsafe BIOS name %q passed: %s", bad, o)
		}
	}
	if o := s.hostOptions("ps2core", coreTable["ps2core"], "/tmp/rw-1", ""); !strings.Contains(o, "shmmb=1024") || strings.Contains(o, "gl=") {
		t.Errorf("test core options: %s", o)
	}
}

// the memory is per core: the PS2 session's cgroup gets 4 GiB, a bigger operator value is kept, the other cores are unchanged
func TestPS2CgroupMemory(t *testing.T) {
	c := config{}
	if got := c.forCore(coreTable["pcsx2"]).sessionMemMB(); got != 4096 {
		t.Errorf("pcsx2 default memory.max %d MiB, want 4096", got)
	}
	if got := c.forCore(coreTable["ppsspp"]).sessionMemMB(); got != defaultSessionMemMB {
		t.Errorf("ppsspp memory.max %d MiB, want the default %d", got, defaultSessionMemMB)
	}
	c.SessionMemMB = 8192
	if got := c.forCore(coreTable["pcsx2"]).sessionMemMB(); got != 8192 {
		t.Errorf("an operator's 8192 was lowered to %d", got)
	}
	c.SessionMemMB = 1024
	if got := c.forCore(coreTable["pcsx2"]).sessionMemMB(); got != 4096 {
		t.Errorf("a 1024 operator value is below what PCSX2 needs: got %d", got)
	}
	if props := strings.Join(c.forCore(coreTable["pcsx2"]).cgroupProps(), " "); !strings.Contains(props, "MemoryMax=4096M") {
		t.Errorf("props %s", props)
	}
}

func TestPS2BigTmpWarning(t *testing.T) {
	c := config{TmpRoot: t.TempDir()}
	if ok, size, _ := tmpfsInfo(c.TmpRoot); ok { // only meaningful where the machine's temp dir is a RAM tmpfs (it is here: /tmp is 16 GiB of RAM)
		c.PS2MaxISOMB = int(size>>20) + 1024
		if w := c.checkBigTmp(); !strings.Contains(w, "tmpfs") {
			t.Errorf("a tmpfs smaller than the image ceiling gave no warning: %q", w)
		}
	}
	c.BigTmp = diskTmp(t)
	if w := c.checkBigTmp(); w != "" {
		t.Errorf("a disk directory warned: %q", w)
	}
	if c.sessionRoot(coreTable["pcsx2"]) != c.BigTmp || c.sessionRoot(coreTable["ppsspp"]) != c.TmpRoot {
		t.Error("session root per core")
	}
}

// Packet -> RetroPad -> what PCSX2's PAD.c reads (the table in pcsx2/PAD/PAD.c: B=Cross, A=Circle, Y=Square, X=Triangle, L=L1, R=R1, L2, R2, L3, R3). The expected
// values are written out here from the core's source, not computed from the component's table.
func TestPS2ButtonMapping(t *testing.T) {
	cases := []struct {
		bit  uint
		want uint16
		name string
	}{
		{4, 1 << 0, "a (south) -> RetroPad B -> Cross"}, {5, 1 << 8, "b (east) -> RetroPad A -> Circle"},
		{6, 1 << 1, "x (west) -> RetroPad Y -> Square"}, {7, 1 << 9, "y (north) -> RetroPad X -> Triangle"},
		{8, 1 << 10, "l1 -> L (L1)"}, {9, 1 << 11, "r1 -> R (R1)"}, {10, 1 << 12, "l2 -> L2"}, {11, 1 << 13, "r2 -> R2"},
		{12, 1 << 14, "l3 -> L3"}, {13, 1 << 15, "r3 -> R3"}, {14, 1 << 3, "start"}, {15, 1 << 2, "select"},
		{0, 1 << 4, "dpad up"}, {1, 1 << 5, "dpad down"}, {2, 1 << 6, "dpad left"}, {3, 1 << 7, "dpad right"}, {16, 0, "guide is ignored"},
	}
	for _, c := range cases {
		if m := retroMaskFor("ps2", 1<<c.bit); m != c.want {
			t.Errorf("%s: got %#x want %#x", c.name, m, c.want)
		}
	}
	if !hasAnalog("ps2") {
		t.Error("hasAnalog(ps2)")
	}
}

// The cards are the only entries of a PS2 package: pack/unpack unit test (the zip rules themselves are in savezip_test.go).
func TestPS2SaveZipOnlyTheCards(t *testing.T) {
	dir := t.TempDir()
	mk := func(n string, size int) {
		p := filepath.Join(dir, n)
		os.MkdirAll(filepath.Dir(p), 0o700)
		b := make([]byte, size)
		copy(b, n)
		os.WriteFile(p, b, 0o600)
	}
	mk("pcsx2/memcards/Mcd001.ps2", 8650752)
	mk("pcsx2/memcards/Mcd002.ps2", 8650752)
	mk("pcsx2/memcards/Mcd003.ps2", 8650752) // not a slot of the package
	mk("pcsx2/memcards/notes.txt", 10)
	mk("pcsx2/bios/scph50004.bin", 1<<20)
	mk("pcsx2/cache/gl_programs.bin", 1000)
	z, err := zipSaveDir(dir, ps2SavePrefix, ps2SaveLimits, 0)
	if err != nil {
		t.Fatal(err)
	}
	cards := ps2Cards(t, z)
	if len(cards) != 2 || cards[ps2SavePrefix+"Mcd001.ps2"] == nil || cards[ps2SavePrefix+"Mcd002.ps2"] == nil {
		t.Fatalf("package entries: %v", len(cards))
	}
	dest := t.TempDir()
	if err := unzipSave(z, dest, ps2SavePrefix, ps2SaveLimits); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "pcsx2/memcards/Mcd001.ps2")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "pcsx2/memcards/Mcd003.ps2")); err == nil {
		t.Fatal("a card outside the two slots was restored")
	}
	// the size limits: an entry above the per-entry bound refuses the package
	big := make([]byte, 80<<20)
	if _, err := parseSaveZip(zipOf(t, map[string][]byte{ps2SavePrefix + "Mcd001.ps2": big}), ps2SavePrefix, ps2SaveLimits); err == nil {
		t.Error("an 80 MiB card was accepted")
	}
	// the PSP rules are unchanged: any name below its prefix
	if pre, lim := saveSpecFor(coreTable["ppsspp"]); pre != pspSavePrefix || lim.Only != nil {
		t.Error("PSP spec changed")
	}
}

// End to end through the DataChannel: every button of the packet reaches the core as the RetroPad bit PCSX2's PAD.c reads, and the four int8 axes of the
// packet arrive as the LEFT stick (axes 0,1) and the RIGHT stick (axes 2,3) of RETRO_DEVICE_ANALOG, scaled x256 (127 -> 32512, -128 -> -32768). The
// ps2core test core writes what it sees into its card file (offset 4096), which this test reads while the session runs.
func TestPS2InputReachesTheCoreBothSticks(t *testing.T) {
	e := ps2Env(t, writeBIOSDir(t, map[string][]byte{"scph50004.bin": syntheticPS2BIOS()}), nil)
	e.totv.romPath = fakePS2ISO(t, 1<<20, "")
	e.create2(t, "inp")
	p := newTestPeer(t)
	defer p.pc.Close()
	p.negotiate(t, e, "inp")
	ss := e.srv.sessions["inp"]
	card := filepath.Join(ss.rwDir, "pcsx2", "memcards", "Mcd001.ps2")
	seen := func() (mask uint16, ax [4]int16, n uint32) {
		f, err := os.Open(card)
		if err != nil {
			return
		}
		defer f.Close()
		var rec [16]byte
		if _, err := f.ReadAt(rec[:], 4096); err != nil {
			return
		}
		mask = binary.LittleEndian.Uint16(rec[0:])
		for i := range ax {
			ax[i] = int16(binary.LittleEndian.Uint16(rec[2+2*i:]))
		}
		return mask, ax, binary.LittleEndian.Uint32(rec[10:])
	}
	waitFor(t, "the core to see the first (neutral) input", 5*time.Second, func() bool { _, _, n := seen(); return n > 0 })
	// packet bit (the order of buttonNames) -> the RetroPad bit PCSX2 reads, written out from pcsx2/PAD/PAD.c
	type btn struct {
		bit  uint
		want uint16
		name string
	}
	for _, b := range []btn{{4, 1 << 0, "cross"}, {5, 1 << 8, "circle"}, {6, 1 << 1, "square"}, {7, 1 << 9, "triangle"}, {8, 1 << 10, "l1"}, {9, 1 << 11, "r1"},
		{10, 1 << 12, "l2"}, {11, 1 << 13, "r2"}, {12, 1 << 14, "l3"}, {13, 1 << 15, "r3"}, {14, 1 << 3, "start"}, {15, 1 << 2, "select"},
		{0, 1 << 4, "up"}, {1, 1 << 5, "down"}, {2, 1 << 6, "left"}, {3, 1 << 7, "right"}} {
		p.sendInput(1 << b.bit)
		waitFor(t, b.name, 3*time.Second, func() bool { m, _, _ := seen(); return m == b.want })
	}
	p.sendInputAxes(0, [4]int8{127, -128, 64, -64})
	waitFor(t, "both sticks", 3*time.Second, func() bool { _, ax, _ := seen(); return ax == [4]int16{32512, -32768, 16384, -16384} })
	p.sendInputAxes(1<<4|1<<7, [4]int8{0, 0, -128, 127}) // cross + triangle with only the RIGHT stick moved
	waitFor(t, "right stick alone with two buttons", 3*time.Second, func() bool {
		m, ax, _ := seen()
		return m == 1<<0|1<<9 && ax == [4]int16{0, 0, -32768, 32512}
	})
	p.sendInputAxes(0, [4]int8{})
	waitFor(t, "neutral again", 3*time.Second, func() bool { m, ax, _ := seen(); return m == 0 && ax == [4]int16{} })
}

func (e *testEnv) create2(t *testing.T, id string) {
	t.Helper()
	if code, m, raw := e.call("POST", "/v1/sessions", e.ps2Body(id), testSecret); code != 201 {
		t.Fatalf("create: %d %v %s", code, m, raw)
	}
}
