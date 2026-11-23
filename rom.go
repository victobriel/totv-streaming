// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"strings"
)

// maxSNESROM is the largest ROM accepted after unpacking (the biggest licensed SNES carts are 6 MiB); the biggest
// N64 carts are 64 MiB.
const (
	maxSNESROM = 8 << 20
	maxN64ROM  = 64 << 20
)

var errNoROMInZip = errors.New("no .sfc/.smc in zip")

func isZip(b []byte) bool { return len(b) >= 4 && bytes.Equal(b[:4], []byte("PK\x03\x04")) }

// unzipROM returns the bytes of the first .sfc/.smc entry of a zip held in memory. Entry names are NEVER used as
// filesystem paths (the ROM is written by the caller to a fixed name), which rules out zip-slip by construction;
// names that are absolute, contain ".." or backslashes are skipped anyway. The size declared in the header is
// checked, and the real decompressed size is limited too (a lying header or a zip bomb cannot exceed max).
func unzipROM(data []byte, max int64) ([]byte, error) { return unzipROMExt(data, max, ".sfc", ".smc") }

func unzipROMExt(data []byte, max int64, exts ...string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		n := f.Name
		if f.FileInfo().IsDir() || strings.HasPrefix(n, "/") || strings.Contains(n, "\\") || hasDotDot(n) {
			continue
		}
		ext := strings.ToLower(path.Ext(n))
		match := false
		for _, e := range exts {
			match = match || ext == e
		}
		if !match {
			continue
		}
		if f.UncompressedSize64 > uint64(max) {
			return nil, errors.New("rom too large")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(io.LimitReader(rc, max+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(out)) > max {
			return nil, errors.New("rom too large")
		}
		if len(out) == 0 {
			continue
		}
		return out, nil
	}
	return nil, errNoROMInZip
}

func hasDotDot(n string) bool {
	for _, p := range strings.Split(n, "/") {
		if p == ".." {
			return true
		}
	}
	return false
}

// normalizeROM replaces a downloaded zip by the ROM inside it (same path, mode 0600). Plain ROMs are left alone.
func normalizeROM(p string) error { return normalizeROMFor(p, "snes") }

func normalizeROMFor(p, platform string) error {
	head := make([]byte, 4)
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	n, _ := io.ReadFull(f, head)
	f.Close()
	if !isZip(head[:n]) {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var rom []byte
	if platform == "n64" {
		rom, err = unzipROMExt(data, maxN64ROM, ".z64", ".n64", ".v64")
	} else {
		rom, err = unzipROM(data, maxSNESROM)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(p, rom, 0o600)
}
