// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestIOReasonSanitized(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
		full bool
	}{
		{&fs.PathError{Op: "write", Path: "/home/someone/secret/game.bin", Err: syscall.EDQUOT}, "disk quota exceeded", true},
		{&fs.PathError{Op: "write", Path: "/home/someone/secret/game.bin", Err: syscall.ENOSPC}, "no space left", true},
		{context.DeadlineExceeded, "timeout", false},
		{errors.New("unexpected thing at /home/someone/x"), "failed", false},
	} {
		r, full := ioReason(tc.err, "download", 5<<20)
		if !strings.Contains(r, tc.want) || full != tc.full || strings.Contains(r, "/home") || !strings.Contains(r, "5 MiB") {
			t.Errorf("ioReason(%v) = %q, %v", tc.err, r, full)
		}
	}
}

func TestDownloadErrorReasons(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nf":
			http.NotFound(w, r)
		default:
			http.Error(w, "x", 500)
		}
	}))
	defer ts.Close()
	c, err := newTotvClient(ts.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "rom")
	_, err = c.download(context.Background(), "/boom", dest, 1<<20)
	if got := downloadReason(err); got != "http status 500" {
		t.Errorf("reason = %q (%v)", got, err)
	}
	_, err = c.download(context.Background(), "/nf", dest, 1<<20)
	if !errors.Is(err, errNotFound) {
		t.Errorf("404 must stay errNotFound: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.download(ctx, "/boom", dest, 1<<20)
	if got := downloadReason(err); !strings.Contains(got, "cancelled") {
		t.Errorf("cancelled reason = %q", got)
	}
	old := freeBytes
	freeBytes = func(string) (uint64, error) { return 1 << 20, nil }
	defer func() { freeBytes = old }()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 4<<20)) }))
	defer ok.Close()
	c2, _ := newTotvClient(ok.URL, "tok")
	_, err = c2.download(context.Background(), "/x", dest, 8<<20)
	if !errors.Is(err, errNoSpace) || !strings.Contains(downloadReason(err), "free space") {
		t.Errorf("no space: %v", err)
	}
}

func TestBigTmpFallback(t *testing.T) {
	// BigTmp set: untouched
	c := config{BigTmp: "/somewhere", TmpRoot: "/dev/shm"}
	if d := c.applyBigTmpFallback(); d != "" || c.BigTmp != "/somewhere" {
		t.Fatalf("explicit BigTmp changed: %q %q", d, c.BigTmp)
	}
	// TmpRoot is a tmpfs: a private disk directory is used (when the cache dir is on a disk; skipped where the whole FS is RAM, as in some CI)
	cache := t.TempDir()
	if ok, _, _ := tmpfsInfo(cache); ok {
		if vt, err := os.MkdirTemp("/var/tmp", "bigtmp-test-"); err == nil {
			t.Cleanup(func() { os.RemoveAll(vt) })
			cache = vt
		}
	}
	if ok, _, _ := tmpfsInfo(cache); ok {
		t.Skip("the test cache directory is itself on a tmpfs")
	}
	t.Setenv("XDG_CACHE_HOME", cache)
	c = config{TmpRoot: "/dev/shm"}
	d := c.applyBigTmpFallback()
	if d != filepath.Join(cache, "totv-streaming", "bigtmp") || c.BigTmp != d {
		t.Fatalf("fallback = %q (%q)", d, c.BigTmp)
	}
	if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("fallback dir mode: %v %v", st, err)
	}
	// TmpRoot is a disk directory: no fallback
	c = config{TmpRoot: cache}
	if d := c.applyBigTmpFallback(); d != "" || c.BigTmp != "" {
		t.Fatalf("fallback on a disk tmp: %q", d)
	}
}
