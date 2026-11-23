// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

// PS2 (PCSX2 libretro core): what is specific to a console that needs a BIOS the project can not ship and an image of several GB.
//
//   - BIOS: the operator provides a directory (TOTV_STREAM_PS2_BIOS_DIR) of dumps of THEIR OWN console (scph*.bin and, optionally, the
//     .rom1/.rom2/.erom/.nvm/.mec files next to them). The component never distributes a BIOS. Before a session it picks one valid file,
//     checks it (the same ROMDIR test the core runs: a "RESET" entry and a "ROMVER" entry), and COPIES it (with the companions) into the
//     session's writable directory, <rw>/pcsx2/bios: the core writes next to the BIOS (.nvm, .mec) and must not be able to touch the
//     operator's directory, which the sandbox never makes readable. No valid BIOS: the session is refused (bios_missing / bios_invalid) and
//     /v1/health says ps2.bios=false.
//   - image: by path (need_fullpath), like the PSP but up to 9 GiB: it is downloaded into a directory of its own in TOTV_STREAM_BIG_TMP (a DISK
//     directory; a RAM tmpfs cannot hold it), read-only for the core.
//   - save: the memory cards <rw>/pcsx2/memcards/Mcd001.ps2 and Mcd002.ps2 (8650752 bytes each), as the same zip as the PSP's (savezip.go).

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	ps2BiosMin       = 1 << 20  // a real ROM0 is 4 MiB (the oldest dumps ~2 MiB); a PS1 BIOS is 512 KiB
	ps2BiosMax       = 16 << 20 // the largest PS2 ROM images (arcade/devel) are 8 MiB; refuse anything above this
	ps2ExtraMax      = 16 << 20 // .rom1/.rom2/.erom/.nvm/.mec companions are tiny (< 2 MiB); a bound against a huge file
	ps2RomdirEntries = 1 << 14  // the RESET entry is looked for in the first 256 KiB (the core scans further, a real BIOS has it at 0x0..0x2000)
	ps2BiosSubdir    = "pcsx2/bios"
	ps2CardSubdir    = "pcsx2/memcards"
)

// ps2SavePrefix is where PCSX2 keeps the memory cards below its system directory (the core's default for "shared" cards).
const ps2SavePrefix = ps2CardSubdir + "/"

// the only two entries of a PS2 save package: slot 1 and slot 2 (PCSX2 names them Mcd001.ps2 / Mcd002.ps2). A card is a raw image of
// 8650752 bytes (16384 pages x 528 with ECC); the limits leave room for the 16/32/64 MiB cards the core can also make.
var ps2SaveLimits = saveLimits{
	MaxFiles: 16 /* entries of the package, ignored ones included; only the two cards are ever packed or extracted */, MaxEntry: 72 << 20, MaxUnpacked: 80 << 20, MaxPacked: 64 << 20,
	Only: map[string]bool{ps2SavePrefix + "Mcd001.ps2": true, ps2SavePrefix + "Mcd002.ps2": true},
}

// saveSpecFor is the zip prefix and limits of a core with a directory save.
func saveSpecFor(ci coreInfo) (string, saveLimits) {
	if ci.platform == "ps2" {
		return ps2SavePrefix, ps2SaveLimits
	}
	return pspSavePrefix, pspSaveLimits
}

// ps2BiosInfo is what ps2BiosScan found: the file the core will use and what is wrong when there is none.
type ps2BiosInfo struct {
	OK      bool     `json:"bios"`
	Reason  string   `json:"reason,omitempty"` // "" when OK; else bios_missing | bios_invalid
	Files   int      `json:"files"`            // regular files in the directory that look like a BIOS image by size
	Valid   int      `json:"valid"`            // of those, the ones with a valid ROMDIR
	Name    string   `json:"-"`                // the chosen file's name (never exposed: it can carry the console model)
	Extras  []string `json:"-"`                // companion files to copy beside it
	Version string   `json:"-"`
}

// ps2BiosNameOK: a plain file name (no separator, no "..", printable, no ';' or '='), safe for the option string and the path join.
func ps2BiosNameOK(n string) bool {
	return n != "" && len(n) <= 128 && n != "." && n != ".." && !strings.ContainsAny(n, "/\\;=\x00") && printable(n)
}

// ps2BiosScan looks in dir for PS2 BIOS dumps. Missing or empty directory (or none configured): bios_missing. Candidates exist but none has a valid
// ROMDIR (a PS1 BIOS, a truncated or corrupt dump): bios_invalid. The first valid one in name order is chosen. Only regular files are looked at
// (a symlink is skipped: the operator's directory is not followed anywhere).
func ps2BiosScan(dir string) ps2BiosInfo {
	var r ps2BiosInfo
	if dir == "" {
		r.Reason = "bios_missing"
		return r
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		r.Reason = "bios_missing"
		return r
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	have := map[string]bool{}
	for _, e := range ents {
		have[e.Name()] = true
	}
	for _, e := range ents {
		if !e.Type().IsRegular() || !ps2BiosNameOK(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		low := strings.ToLower(e.Name())
		if strings.HasSuffix(low, ".nvm") || strings.HasSuffix(low, ".mec") || strings.HasSuffix(low, ".rom1") || strings.HasSuffix(low, ".rom2") || strings.HasSuffix(low, ".erom") {
			continue // a companion, not a BIOS
		}
		r.Files++
		if info.Size() < ps2BiosMin || info.Size() > ps2BiosMax {
			continue
		}
		ver, err := ps2BiosCheck(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		r.Valid++
		if r.Name == "" {
			r.Name, r.Version = e.Name(), ver
			stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			for _, suf := range []string{e.Name() + ".rom1", e.Name() + ".rom2", e.Name() + ".erom", stem + ".rom1", stem + ".rom2", stem + ".erom", stem + ".nvm", stem + ".mec", e.Name() + ".nvm", e.Name() + ".mec"} {
				if have[suf] {
					r.Extras = append(r.Extras, suf)
				}
			}
		}
	}
	switch {
	case r.Valid > 0:
		r.OK = true
	case r.Files > 0:
		r.Reason = "bios_invalid"
	default:
		r.Reason = "bios_missing"
	}
	return r
}

// ps2BiosCheck repeats the core's own test (BiosTools.c LoadBiosVersion): the ROM image starts with the ROMDIR table, whose entries are
// 16 bytes (name[10], extinfo u16, size u32); a "RESET" entry is found, then the entries up to the end marker are walked adding up the (16-byte
// aligned) sizes, and a "ROMVER" entry must be there. Returns the 14-character ROMVER (version, region, date) for the log. The file is read at most
// 256 KiB (the table) plus the ROMVER bytes.
func ps2BiosCheck(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	head := make([]byte, ps2RomdirEntries*16)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	head = head[:n/16*16]
	reset := -1
	for i := 0; i+16 <= len(head); i += 16 {
		if cstr(head[i:i+10]) == "RESET" {
			reset = i
			break
		}
	}
	if reset < 0 {
		return "", errors.New("no RESET entry: not a PS2 BIOS image")
	}
	// entry 0 is RESET itself: its file starts at offset 0
	var off int64
	size := func(e []byte) int64 { return int64(binary.LittleEndian.Uint32(e[12:16])) }
	adv := func(sz int64) int64 {
		if sz%16 == 0 {
			return sz
		}
		return (sz + 16) &^ 15
	}
	for p := reset; p+16 <= len(head); p += 16 {
		e := head[p : p+16]
		name := cstr(e[:10])
		if e[0] == 0 || strings.IndexByte(string(e[:10]), 0) < 0 { // end marker, or a name without its terminator (the core stops there too)
			break
		}
		if name == "ROMVER" {
			if st.Size() < off+14 {
				return "", errors.New("ROMVER beyond the end of the file")
			}
			v := make([]byte, 14)
			if _, err := f.ReadAt(v, off); err != nil {
				return "", err
			}
			for _, c := range v {
				if c < 0x20 || c > 0x7e {
					return "", errors.New("ROMVER is not text")
				}
			}
			return string(v), nil
		}
		off += adv(size(e))
	}
	return "", errors.New("no ROMVER entry")
}

func cstr(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// ps2Health is `ps2` of /v1/health: the BIOS state (re-read at every request: the operator may add the files while the component runs).
func (s *server) ps2Health() any {
	if !s.ps2Present() {
		return nil // no PS2 core installed here: the field says nothing
	}
	r := ps2BiosScan(s.cfg.PS2BiosDir)
	m := map[string]any{"bios": r.OK, "bios_dir_set": s.cfg.PS2BiosDir != "", "bios_files": r.Files, "bios_valid": r.Valid, "max_iso_mb": s.cfg.ps2MaxISO() >> 20}
	if !r.OK {
		m["reason"] = r.Reason
	}
	m["big_tmp"] = s.cfg.bigTmp()
	return m
}

// ps2Present: a PS2 core (the real one, or the test core in dev mode) is installed.
func (s *server) ps2Present() bool {
	for name, ci := range coreTable {
		if ci.platform != "ps2" {
			continue
		}
		if _, ok := s.coreAllowed(name); !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.cfg.CoresDir, name+"_libretro.so")); err == nil {
			return true
		}
	}
	return false
}

// installPS2BIOS copies the chosen BIOS (and its companions) from the operator's directory to <rw>/pcsx2/bios. The copy is made by the parent,
// before the sandbox: the operator's directory is never readable for the core. Files are created O_EXCL with 0600; sizes are bounded.
func installPS2BIOS(srcDir, rw string, r ps2BiosInfo) error {
	dst := filepath.Join(rw, filepath.FromSlash(ps2BiosSubdir))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, n := range append([]string{r.Name}, r.Extras...) {
		if !ps2BiosNameOK(n) {
			continue
		}
		if err := copyRegular(filepath.Join(srcDir, n), filepath.Join(dst, n), ps2BiosMax); err != nil {
			return err
		}
	}
	return nil
}

// copyRegular copies one regular file (never a link: O_NOFOLLOW) of at most max bytes to dst (O_EXCL).
func copyRegular(src, dst string, max int64) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return errors.New("not a regular file or too large")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, max+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fs.ErrInvalid
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

// ps2ROMExt names the image by its magic (the core picks its reader by extension): CHD ("MComprHD"), CSO ("CISO"), ZSO ("ZISO"), else a plain ISO
// (or a 2352-byte BIN: the core validates the volume descriptor itself and refuses the rest). A zip or gzip is refused: it would have to be
// unpacked to GB of disk.
func ps2ROMExt(head []byte) (string, error) {
	switch {
	case len(head) < 4:
		return "", errors.New("rom too small")
	case len(head) >= 8 && string(head[:8]) == "MComprHD":
		return ".chd", nil
	case string(head[:4]) == "CISO":
		return ".cso", nil
	case string(head[:4]) == "ZISO":
		return ".zso", nil
	case isZip(head) || (head[0] == 0x1f && head[1] == 0x8b):
		return "", errors.New("zip/gzip not supported for this platform")
	}
	return ".iso", nil
}

// bigTmp is where the session directory of a core with big images goes: TOTV_STREAM_BIG_TMP, else TOTV_STREAM_TMP.
func (c config) bigTmp() string {
	if c.BigTmp != "" {
		return c.BigTmp
	}
	return c.TmpRoot
}

// ps2MaxISO is the download ceiling of a PS2 image (TOTV_STREAM_PS2_MAX_ISO_MB, default 9 GiB).
func (c config) ps2MaxISO() int64 {
	if c.PS2MaxISOMB > 0 {
		return int64(c.PS2MaxISOMB) << 20
	}
	return maxPS2ROM
}

// romMax is the download limit for a core's ROM/image.
func (c config) romMax(ci coreInfo) int64 {
	if ci.platform == "ps2" {
		return c.ps2MaxISO()
	}
	return ci.maxROM
}

// sessionRoot is the directory a core's session directory is created in.
func (c config) sessionRoot(ci coreInfo) string {
	if ci.bigROM {
		return c.bigTmp()
	}
	return c.TmpRoot
}

// checkBigTmp: the directory for big images must NOT be a RAM tmpfs of less than the image ceiling (9 GiB would not fit in RAM next to the
// emulator): a tmpfs there is refused unless it is bigger than the ceiling plus the session's own memory, and a warning is logged at start.
// A plain disk directory is the intended case. Returns a warning text ("" = fine).
func (c config) checkBigTmp() string {
	d := c.bigTmp()
	isTmpfs, size, err := tmpfsInfo(d)
	if err != nil {
		return "TOTV_STREAM_BIG_TMP " + d + " is not usable: " + err.Error()
	}
	if isTmpfs && int64(size) < c.ps2MaxISO()+int64(ps2SessionMemMB)<<20 {
		return "TOTV_STREAM_BIG_TMP (" + d + ") is a tmpfs of " + itoa64(int64(size>>20)) + " MiB: a PS2 image of up to " + itoa64(c.ps2MaxISO()>>20) + " MiB would be held in RAM; point TOTV_STREAM_BIG_TMP at a disk directory"
	}
	return ""
}

// bigRootIsTmpfs: the directory big images go to is a RAM tmpfs.
func (c config) bigRootIsTmpfs() bool {
	ok, _, err := tmpfsInfo(c.bigTmp())
	return err == nil && ok
}

// applyBigTmpFallback: TOTV_STREAM_BIG_TMP is unset and TOTV_STREAM_TMP is a tmpfs. A tmpfs reports free space in statfs and can still refuse
// the write (per-user quota: EDQUOT at ~2.7 GiB on a 16 GiB /tmp), and a PS2 image would sit in RAM. So big images go to a private DISK directory
// under $XDG_CACHE_HOME (else ~/.cache), mode 0700, emptied at the end of each session and at boot (cleanOrphans). Returns the directory ("" =
// no fallback was needed or possible: the tmpfs stays, and an image that does not fit fails early with rom_too_large_tmp).
func (c *config) applyBigTmpFallback() string {
	if c.BigTmp != "" || !c.bigRootIsTmpfs() {
		return ""
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" || !filepath.IsAbs(base) {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return ""
		}
		base = filepath.Join(h, ".cache")
	}
	d := filepath.Join(base, "totv-streaming", "bigtmp")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return ""
	}
	if err := os.Chmod(d, 0o700); err != nil {
		return ""
	}
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		return ""
	}
	if ok, _, err := tmpfsInfo(d); err != nil || ok {
		return ""
	}
	c.BigTmp = d
	return d
}
