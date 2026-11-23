// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSecondHelloAndSRAMLimits drives readLoop with a fake child: no panic on a second hello, 'S' bounded by the hello.
func TestSecondHelloAndSRAMLimits(t *testing.T) {
	e := newEnv(t, nil)
	msg := func(kind byte, p []byte) []byte {
		h := make([]byte, 8)
		h[0] = kind
		binary.LittleEndian.PutUint32(h[4:], uint32(len(p)))
		return append(h, p...)
	}
	hello := func(fps float64, sram uint32, flags uint32) []byte {
		p := make([]byte, 32)
		binary.LittleEndian.PutUint64(p, mathBits(fps))
		binary.LittleEndian.PutUint64(p[8:], mathBits(48000))
		binary.LittleEndian.PutUint32(p[24:], sram)
		binary.LittleEndian.PutUint32(p[28:], flags)
		return msg('H', p)
	}
	run := func(parts ...[]byte) *session {
		ss := newSession(e.srv, createReq{SessionID: "x"}, "c", "", nil, "")
		ss.stdout = io.NopCloser(bytes.NewReader(bytes.Join(parts, nil)))
		go ss.readLoop()
		select {
		case <-ss.exited:
		case <-time.After(5 * time.Second):
			t.Fatal("readLoop did not end")
		}
		return ss
	}
	ss := run(hello(60, 8, 7), hello(30, 99, 0), msg('S', make([]byte, 8)))
	if ss.fps != 60 || ss.flags != 7 || ss.sramSize.Load() != 8 {
		t.Fatalf("second hello changed state: fps=%v flags=%d sram=%d", ss.fps, ss.flags, ss.sramSize.Load())
	}
	if len(ss.sram) != 8 {
		t.Fatalf("valid S not accepted (%d)", len(ss.sram))
	}
	if ss = run(hello(60, 8, 7), msg('S', make([]byte, 9))); len(ss.sram) != 0 {
		t.Fatal("oversized S accepted")
	}
	if ss = run(msg('S', make([]byte, 8))); len(ss.sram) != 0 {
		t.Fatal("S before hello accepted")
	}
	if ss = run(hello(60, 0, 7), msg('S', make([]byte, 1))); len(ss.sram) != 0 {
		t.Fatal("S accepted for a core without save RAM")
	}
	if ss = run(msg('V', make([]byte, 10))); ss.vFrames.Load() != 0 {
		t.Fatal("video before hello / wrong size accepted")
	}
}

func TestMaxChildMsg(t *testing.T) {
	if maxChildMsg('S', 100, 8, true) != 8 || maxChildMsg('S', 100, 8, false) != -1 || maxChildMsg('?', 100, 8, true) != -1 ||
		maxChildMsg('E', 100, 8, false) != 4096 {
		t.Fatal("limits wrong")
	}
}

func mathBits(f float64) uint64 { return math.Float64bits(f) }

// Regression: a core that writes text to stdout while exiting (snes9x: "Map_LoROMMap") used to be read by the parent
// as a frame header ('M' of 1330802508 bytes) on every stop. A full stop must log no protocol violation and still save.
func TestStopHasNoProtocolViolation(t *testing.T) {
	var buf syncBuf
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	e := newEnv(t, nil)
	e.create("sx")
	time.Sleep(500 * time.Millisecond)
	if code, m, _ := e.call("DELETE", "/v1/sessions/sx", nil, testSecret); code != 200 || m["ok"] != true {
		t.Fatalf("delete %d %v", code, m)
	}
	out := buf.String()
	if strings.Contains(out, "violates the protocol") {
		t.Fatalf("protocol violation on stop:\n%s", out)
	}
	if !strings.Contains(out, "stopped saved=true") {
		t.Fatalf("expected saved=true:\n%s", out)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
