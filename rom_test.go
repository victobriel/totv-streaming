package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func mkZip(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, n := range order {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(files[n])
	}
	zw.Close()
	return b.Bytes()
}

func TestUnzipROM(t *testing.T) {
	rom := bytes.Repeat([]byte{7}, 4096)
	files := map[string][]byte{"readme.txt": []byte("x"), "../../evil.sfc": []byte("evil"), "/abs.sfc": []byte("abs"), "game.SMC": rom, "b.sfc": []byte("later")}
	z := mkZip(t, files, []string{"readme.txt", "../../evil.sfc", "/abs.sfc", "game.SMC", "b.sfc"})
	got, err := unzipROM(z, maxSNESROM)
	if err != nil || !bytes.Equal(got, rom) {
		t.Fatalf("want first safe .smc (zip-slip names skipped): err=%v len=%d", err, len(got))
	}
	if _, err := unzipROM(mkZip(t, map[string][]byte{"a.txt": []byte("x")}, []string{"a.txt"}), maxSNESROM); err != errNoROMInZip {
		t.Fatalf("no rom: %v", err)
	}
	// size limit (declared and real) / zip bomb
	big := bytes.Repeat([]byte{0}, 3<<20)
	if _, err := unzipROM(mkZip(t, map[string][]byte{"a.sfc": big}, []string{"a.sfc"}), 1<<20); err == nil {
		t.Fatal("oversized rom accepted")
	}
	if _, err := unzipROM([]byte("PK\x03\x04garbage"), maxSNESROM); err == nil {
		t.Fatal("corrupt zip accepted")
	}
}

func TestNormalizeROM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "game.sfc")
	os.WriteFile(p, []byte("plain rom bytes"), 0o600)
	if err := normalizeROM(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "plain rom bytes" {
		t.Fatal("plain ROM was changed")
	}
	rom := bytes.Repeat([]byte{9}, 1024)
	os.WriteFile(p, mkZip(t, map[string][]byte{"x.sfc": rom}, []string{"x.sfc"}), 0o600)
	if err := normalizeROM(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, rom) {
		t.Fatal("zip not unpacked")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("extra files written: %v", ents)
	}
}

// firstFrame runs retro-emu directly with a core and returns the first I420 frame.
func firstFrame(t *testing.T, core, rom string) []byte {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("bin/retro-emu", core, rom, "-", dir, "640", "480")
	cmd.Env = []string{}
	out, _ := cmd.StdoutPipe()
	in, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	for {
		h := make([]byte, 8)
		if _, err := io.ReadFull(out, h); err != nil {
			t.Fatal(err)
		}
		n := binary.LittleEndian.Uint32(h[4:])
		p := make([]byte, n)
		if _, err := io.ReadFull(out, p); err != nil {
			t.Fatal(err)
		}
		switch h[0] {
		case 'E':
			t.Fatalf("emu error: %s", p)
		case 'V':
			return p
		}
	}
}

// TestPixelFormats: the same picture through XRGB8888, RGB565 and 0RGB1555 must give (almost) the same I420.
func TestPixelFormats(t *testing.T) {
	rom := filepath.Join(t.TempDir(), "x")
	os.WriteFile(rom, []byte("x"), 0o600)
	ref := firstFrame(t, "bin/testcore_libretro.so", rom)
	for _, c := range []string{"bin/testcore565_libretro.so", "bin/testcore1555_libretro.so"} {
		f := firstFrame(t, c, rom)
		if len(f) != len(ref) {
			t.Fatalf("%s: size %d vs %d", c, len(f), len(ref))
		}
		maxd, sum := 0, 0
		for i := range f {
			d := int(f[i]) - int(ref[i])
			if d < 0 {
				d = -d
			}
			if d > maxd {
				maxd = d
			}
			sum += d
		}
		t.Logf("%s: max diff %d, mean %.2f", c, maxd, float64(sum)/float64(len(f)))
		if maxd > 12 {
			t.Fatalf("%s: color conversion off (max diff %d)", c, maxd)
		}
	}
}
