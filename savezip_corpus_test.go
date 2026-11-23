// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The same synthetic zips the Totv tests its validateSaveZip with (server/test/fixtures/save-zips/, copied here: no game content).
// ok-* must load, bad-* must be refused; maxUnpacked is the limit the corpus was built for.
func TestSaveZipCorpusParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/save-zips/manifest.json")
	if err != nil {
		t.Skip("no corpus")
	}
	var m []struct {
		File, Expect, Why string
		MaxUnpacked       int64
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, e := range m {
		t.Run(e.File, func(t *testing.T) {
			z, err := os.ReadFile(filepath.Join("testdata/save-zips", e.File))
			if err != nil {
				t.Fatal(err)
			}
			lim := pspSaveLimits
			lim.MaxUnpacked = e.MaxUnpacked
			if lim.MaxEntry > e.MaxUnpacked {
				lim.MaxEntry = e.MaxUnpacked
			}
			dest := t.TempDir()
			err = unzipSave(z, dest, pspSavePrefix, lim)
			if (err == nil) != (e.Expect == "accept") {
				t.Errorf("%s: component says err=%v, the Totv expects %s (%s)", e.File, err, e.Expect, e.Why)
			}
			if err == nil { // whatever loads must be below PSP/SAVEDATA/ and nothing else
				_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
					rel, _ := filepath.Rel(dest, p)
					if err == nil && !d.IsDir() && !safeZipName(filepath.ToSlash(rel), pspSavePrefix) {
						t.Errorf("%s: wrote %q outside the prefix", e.File, rel)
					}
					return nil
				})
			}
		})
	}
}
