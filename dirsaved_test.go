package main

import "testing"

// stopSaved for directory-save cores (PSP/PS2) means "a package that differs from the one the session started from was accepted".
func TestDirSaveSent(t *testing.T) {
	base := []byte("zip-at-start")
	neu := []byte("zip-after-play")
	cases := []struct {
		name          string
		base, up, cur []byte
		want          bool
	}{
		{"no save at all", nil, nil, nil, false},
		{"unchanged: only the downloaded package", base, base, base, false},
		{"unchanged, packed identical, nothing sent", base, base, append([]byte(nil), base...), false},
		{"new package accepted", base, neu, neu, true},
		{"first save accepted (no save before)", nil, neu, neu, true},
		{"new package, upload failed", base, base, neu, false},
		{"new package, nothing ever uploaded", nil, nil, neu, false},
		{"changed again after the last upload", base, neu, []byte("later"), false},
	}
	for _, c := range cases {
		s := &session{dirBase: c.base, sramUploaded: c.up, sram: c.cur}
		if got := s.dirSaveSent(); got != c.want {
			t.Errorf("%s: dirSaveSent=%v, want %v", c.name, got, c.want)
		}
	}
}
