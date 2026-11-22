package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func mkSaveTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	w := func(rel, data string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Minute)
		_ = os.Chtimes(p, old, old)
	}
	w("PSP/SAVEDATA/ULUS10001DATA00/DATA.BIN", strings.Repeat("save", 1000))
	w("PSP/SAVEDATA/ULUS10001DATA00/PARAM.SFO", "sfo")
	w("PSP/SAVEDATA/ULUS10002/ICON0.PNG", "png")
	w("PSP/SYSTEM/CACHE/shaders.bin", "NOT A SAVE")
	w("PSP/GAME/evil/EBOOT.PBP", "NOT A SAVE")
	return root
}

func rawZip(t *testing.T, entries map[string]string, mutate func(*zip.FileHeader)) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, d := range entries {
		h := &zip.FileHeader{Name: n, Method: zip.Deflate}
		if mutate != nil {
			mutate(h)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(d))
	}
	_ = zw.Close()
	return b.Bytes()
}

func TestSaveZipRoundTripOnlySavedata(t *testing.T) {
	root := mkSaveTree(t)
	z, err := zipSaveDir(root, pspSavePrefix, pspSaveLimits, 0)
	if err != nil || len(z) == 0 {
		t.Fatalf("zipSaveDir: %v len=%d", err, len(z))
	}
	ents, err := parseSaveZip(z, pspSavePrefix, pspSaveLimits)
	if err != nil || len(ents) != 3 {
		t.Fatalf("parse: %v %d entries", err, len(ents))
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.name, pspSavePrefix) {
			t.Errorf("entry %q outside the prefix (shader cache / GAME must never be packed)", e.name)
		}
	}
	dest := t.TempDir()
	if err := unzipSave(z, dest, pspSavePrefix, pspSaveLimits); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "PSP/SAVEDATA/ULUS10001DATA00/DATA.BIN"))
	if string(got) != strings.Repeat("save", 1000) {
		t.Fatal("content differs after the round trip")
	}
	if _, err := os.Stat(filepath.Join(dest, "PSP/SYSTEM")); err == nil {
		t.Fatal("PSP/SYSTEM was restored")
	}
	z2, _ := zipSaveDir(root, pspSavePrefix, pspSaveLimits, 0)
	if !bytes.Equal(z, z2) {
		t.Fatal("same tree must give the same bytes (the 'changed?' comparison relies on it)")
	}
}

func TestSaveZipEmptyAndBusy(t *testing.T) {
	if z, err := zipSaveDir(t.TempDir(), pspSavePrefix, pspSaveLimits, 0); z != nil || err != nil {
		t.Fatalf("no savedata must give nil, nil: %v %v", z, err)
	}
	root := mkSaveTree(t)
	p := filepath.Join(root, "PSP/SAVEDATA/ULUS10002/ICON0.PNG")
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	if _, err := zipSaveDir(root, pspSavePrefix, pspSaveLimits, 5*time.Second); !errors.Is(err, errSaveBusy) {
		t.Fatalf("a file written a moment ago must be 'busy', got %v", err)
	}
}

func TestSaveZipSkipsSymlinksAndLimits(t *testing.T) {
	root := mkSaveTree(t)
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte("TOKEN"), 0o600)
	_ = os.Symlink(secret, filepath.Join(root, "PSP/SAVEDATA/ULUS10002/link"))
	_ = os.Symlink(filepath.Dir(secret), filepath.Join(root, "PSP/SAVEDATA/dirlink"))
	z, err := zipSaveDir(root, pspSavePrefix, pspSaveLimits, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(z, []byte("TOKEN")) {
		t.Fatal("a symlink target was packed")
	}
	ents, _ := parseSaveZip(z, pspSavePrefix, pspSaveLimits)
	for _, e := range ents {
		if strings.Contains(e.name, "link") {
			t.Fatalf("symlink entry %q", e.name)
		}
	}
	if _, err := zipSaveDir(root, pspSavePrefix, saveLimits{MaxFiles: 2, MaxEntry: 1 << 20, MaxUnpacked: 1 << 20, MaxPacked: 1 << 20}, 0); err == nil {
		t.Fatal("file count limit not enforced")
	}
	if _, err := zipSaveDir(root, pspSavePrefix, saveLimits{MaxFiles: 10, MaxEntry: 100, MaxUnpacked: 1 << 20, MaxPacked: 1 << 20}, 0); err == nil {
		t.Fatal("entry size limit not enforced")
	}
	if _, err := zipSaveDir(root, pspSavePrefix, saveLimits{MaxFiles: 10, MaxEntry: 1 << 20, MaxUnpacked: 3000, MaxPacked: 1 << 20}, 0); err == nil {
		t.Fatal("total size limit not enforced")
	}
}

func TestUnzipSaveRejectsHostileZips(t *testing.T) {
	ok := pspSavePrefix + "X/DATA"
	cases := map[string][]byte{
		"zip-slip ..":         rawZip(t, map[string]string{pspSavePrefix + "../../etc/x": "x"}, nil),
		"zip-slip start ..":   rawZip(t, map[string]string{"../PSP/SAVEDATA/x": "x"}, nil),
		"absolute":            rawZip(t, map[string]string{"/PSP/SAVEDATA/x": "x"}, nil),
		"backslash":           rawZip(t, map[string]string{pspSavePrefix + `a\..\b`: "x"}, nil),
		"drive":               rawZip(t, map[string]string{"C:/PSP/SAVEDATA/x": "x"}, nil),
		"slip outside prefix": rawZip(t, map[string]string{"PSP/GAME/../../x": "x"}, nil),
		"symlink outside prefix": rawZip(t, map[string]string{"PSP/GAME/link": "/etc/passwd"}, func(h *zip.FileHeader) {
			h.SetMode(0o777 | os.ModeSymlink)
		}),
		"slip dir entry":  rawZip(t, map[string]string{"../x/": ""}, nil),
		"empty component": rawZip(t, map[string]string{pspSavePrefix + "a//b": "x"}, nil),
		"nul":             rawZip(t, map[string]string{pspSavePrefix + "a\x00b": "x"}, nil),
		"dot":             rawZip(t, map[string]string{pspSavePrefix + "./a": "x"}, nil),
		"symlink": rawZip(t, map[string]string{ok: "/etc/passwd"}, func(h *zip.FileHeader) {
			h.SetMode(0o777 | os.ModeSymlink)
		}),
		"file and dir": rawZip(t, map[string]string{pspSavePrefix + "a": "x", pspSavePrefix + "a/b": "y"}, nil),
		"empty":        rawZip(t, nil, nil),
		"garbage":      []byte("this is not a zip, just text, long enough to pass the size check"),
	}
	for name, z := range cases {
		dest := t.TempDir()
		if err := unzipSave(z, dest, pspSavePrefix, pspSaveLimits); err == nil {
			t.Errorf("%s: accepted", name)
		}
		var n int
		_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				n++
			}
			return nil
		})
		if n != 0 {
			t.Errorf("%s: %d files left behind", name, n)
		}
	}
	if _, err := os.Stat("/etc/x"); err == nil {
		t.Fatal("zip-slip wrote /etc/x")
	}
}

// M3: what is not ours is ignored (directory entries, names outside PSP/SAVEDATA/), never extracted, and does not refuse the package.
func TestUnzipSaveIgnoresWhatIsNotSavedata(t *testing.T) {
	z := rawZip(t, map[string]string{
		pspSavePrefix:                  "",
		"PSP/":                         "",
		pspSavePrefix + "G1/":          "",
		pspSavePrefix + "G1/DATA.BIN":  "save",
		"PSP/GAME/evil/EBOOT.PBP":      "homebrew",
		"PSP/SYSTEM/CACHE/shaders.bin": "cache",
		"ppsspp.ini":                   "ini",
	}, func(h *zip.FileHeader) {
		if strings.HasSuffix(h.Name, "/") {
			h.Method = zip.Store
		}
	})
	dest := t.TempDir()
	if err := unzipSave(z, dest, pspSavePrefix, pspSaveLimits); err != nil {
		t.Fatalf("a package with directory entries and foreign names must load: %v", err)
	}
	var got []string
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dest, p)
			got = append(got, rel)
		}
		return nil
	})
	if len(got) != 1 || got[0] != "PSP/SAVEDATA/G1/DATA.BIN" {
		t.Fatalf("only the savedata may be written, got %v", got)
	}
	// nothing but foreign names: accepted, nothing written
	only := rawZip(t, map[string]string{"PSP/GAME/x/EBOOT.PBP": "x"}, nil)
	if err := unzipSave(only, t.TempDir(), pspSavePrefix, pspSaveLimits); err != nil {
		t.Fatalf("foreign-only package: %v", err)
	}
	// ...but an over-limit entry that WOULD be extracted is still refused
	big := rawZip(t, map[string]string{pspSavePrefix + "X/BIG": strings.Repeat("a", 5000)}, nil)
	if err := unzipSave(big, t.TempDir(), pspSavePrefix, saveLimits{MaxFiles: 10, MaxEntry: 1000, MaxUnpacked: 1 << 20, MaxPacked: 1 << 20}); err == nil {
		t.Error("entry over the limit accepted")
	}
}

// zipSaveDir never follows a link and never blocks on a FIFO swapped in for a file
func TestZipSaveDirFifoAndLinkedDirectory(t *testing.T) {
	root := mkSaveTree(t)
	if err := syscall.Mkfifo(filepath.Join(root, "PSP/SAVEDATA/ULUS10002/pipe"), 0o600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, err := zipSaveDir(root, pspSavePrefix, pspSaveLimits, 0); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("zipSaveDir blocked on a FIFO")
	}
	// the savedata directory itself is a link to somewhere else: nothing is packed
	other := t.TempDir()
	_ = os.MkdirAll(filepath.Join(other, "SAVEDATA/X"), 0o700)
	_ = os.WriteFile(filepath.Join(other, "SAVEDATA/X/f"), []byte("outside"), 0o600)
	r2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(r2, "PSP"), 0o700)
	_ = os.Symlink(filepath.Join(other, "SAVEDATA"), filepath.Join(r2, "PSP/SAVEDATA"))
	if z, _ := zipSaveDir(r2, pspSavePrefix, pspSaveLimits, 0); z != nil { // nil + an error ("path escapes from parent") or nil + nil: both fail closed
		t.Fatal("a linked SAVEDATA directory was followed")
	}
}

// the declared size and the real one must agree, the local header name must equal the central one, the limits count real bytes
func TestUnzipSaveSizeAndHeaderTricks(t *testing.T) {
	name := pspSavePrefix + "X/DATA"
	good := rawZip(t, map[string]string{name: strings.Repeat("a", 5000)}, nil)
	if err := unzipSave(good, t.TempDir(), pspSavePrefix, pspSaveLimits); err != nil {
		t.Fatalf("good zip: %v", err)
	}
	// lying central directory: usize smaller than the real data
	lie := append([]byte(nil), good...)
	i := bytes.Index(lie, []byte{0x50, 0x4b, 0x01, 0x02})
	binary.LittleEndian.PutUint32(lie[i+24:], 100)
	if err := unzipSave(lie, t.TempDir(), pspSavePrefix, pspSaveLimits); err == nil {
		t.Error("declared size smaller than the real one was accepted")
	}
	// bomb: honest header, over the limit
	bomb := rawZip(t, map[string]string{name: strings.Repeat("\x00", 4<<20)}, nil)
	if err := unzipSave(bomb, t.TempDir(), pspSavePrefix, saveLimits{MaxFiles: 10, MaxEntry: 1 << 20, MaxUnpacked: 1 << 20, MaxPacked: 1 << 20}); err == nil {
		t.Error("over-limit entry accepted")
	}
	// local header name differs from the central one
	mm := append([]byte(nil), good...)
	j := bytes.Index(mm, []byte(name))
	mm[j+len(name)-1] = 'Z' // first occurrence is the local header
	if err := unzipSave(mm, t.TempDir(), pspSavePrefix, pspSaveLimits); err == nil {
		t.Error("local/central name mismatch accepted")
	}
	// too many entries
	many := map[string]string{}
	for k := 0; k < 30; k++ {
		many[pspSavePrefix+"X/f"+string(rune('a'+k%26))+string(rune('a'+k/26))] = "x"
	}
	if err := unzipSave(rawZip(t, many, nil), t.TempDir(), pspSavePrefix, saveLimits{MaxFiles: 10, MaxEntry: 1 << 20, MaxUnpacked: 1 << 20, MaxPacked: 1 << 20}); err == nil {
		t.Error("entry count limit not enforced")
	}
	// the destination is never followed through an existing symlink
	dest := t.TempDir()
	outside := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dest, "PSP"), 0o700)
	_ = os.Symlink(outside, filepath.Join(dest, "PSP/SAVEDATA"))
	_ = unzipSave(good, dest, pspSavePrefix, pspSaveLimits)
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Error("extraction followed a symlink out of the destination")
	}
}

// Cross-check with the Totv's real validator (server/games/saves.js, validateSaveZip) when the Totv repository is around:
// what the component produces must be accepted. TOTV_SAVES_JS=<path of saves.js>.
func TestSaveZipAcceptedByTotvValidator(t *testing.T) {
	js := os.Getenv("TOTV_SAVES_JS")
	if js == "" {
		t.Skip("TOTV_SAVES_JS not set")
	}
	z, err := zipSaveDir(mkSaveTree(t), pspSavePrefix, pspSaveLimits, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "save.zip")
	_ = os.WriteFile(f, z, 0o600)
	script := `import fs from 'node:fs'; const S = await import(process.argv[1]); const b = fs.readFileSync(process.argv[2]);
await S.validateSaveZip(b, 64*1024*1024*4); await S.validateSaveZip({path: process.argv[2], size: b.length}, 64*1024*1024*4); console.log('ok');`
	cmd := exec.Command("node", "--input-type=module", "-e", script, "file://"+js, f)
	cmd.Dir = filepath.Dir(filepath.Dir(js)) // the server/ directory: db.js and config.js resolve from there
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("Totv validateSaveZip refused the package: %v\n%s", err, out)
	}
}
