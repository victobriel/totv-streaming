// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"testing"
	"time"
)

func TestMaxSessionsClamp(t *testing.T) {
	for in, want := range map[int]int{-3: 1, 0: 1, 1: 1, 2: 2, 4: 4, 5: 4, 99: 4} {
		if got := (config{MaxSessions: in}).maxSessions(); got != want {
			t.Fatalf("maxSessions(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestMuxPoolRefcount(t *testing.T) {
	var p muxPool
	a, err := p.acquire("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.acquire("127.0.0.1", 0)
	if err != nil || a != b || p.open() != 1 {
		t.Fatalf("second acquire must share the socket: %v %v %d", err, a == b, p.open())
	}
	p.release(a)
	if p.open() != 1 {
		t.Fatal("socket closed while a session still holds it")
	}
	p.release(b)
	if p.open() != 0 {
		t.Fatal("socket not closed after the last release")
	}
	p.release(nil)
	c, err := p.acquire("127.0.0.1", 0) // reopened for the next session
	if err != nil || p.open() != 1 {
		t.Fatal(err)
	}
	p.release(c)
}

// Two sessions at once on ONE UDP socket: both get video, input goes only to its own session, a third is refused (busy/max_sessions),
// ending one leaves the other playing, and the socket closes with the last session.
func TestTwoSessionsShareOneUDPMux(t *testing.T) {
	e := newEnv(t, func(c *config) { c.MaxSessions = 2 })
	e.create("a1")
	e.create("b2")
	if code, m, _ := e.call("POST", "/v1/sessions", e.createBody("c3"), testSecret); code != 409 || m["error"] != "busy" || m["reason"] != "max_sessions" || m["max"] != float64(2) {
		t.Fatalf("third session: %d %v", code, m)
	}
	pa, pb := newTestPeer(t), newTestPeer(t)
	defer pa.pc.Close()
	defer pb.pc.Close()
	pa.negotiate(t, e, "a1")
	pb.negotiate(t, e, "b2")
	if n := e.srv.muxes.open(); n != 1 {
		t.Fatalf("sockets open = %d, want 1 shared", n)
	}
	waitFor(t, "video on both", 10*time.Second, func() bool {
		return pa.vPackets.Load() > 30 && pb.vPackets.Load() > 30 && pa.aPackets.Load() > 20 && pb.aPackets.Load() > 20
	})

	// input isolation
	pa.sendInput(1 << 4)
	waitFor(t, "input applied on a1", 3*time.Second, func() bool { return statI(t, e, "a1", "inputApplied") >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := statI(t, e, "b2", "inputApplied"); n != 0 {
		t.Fatalf("input of a1 reached b2 (%d)", n)
	}
	pb.sendInput(1 << 5)
	pb.sendInput(1 << 6)
	waitFor(t, "input applied on b2", 3*time.Second, func() bool { return statI(t, e, "b2", "inputApplied") >= 1 })
	if n := statI(t, e, "a1", "inputApplied"); n != 1 {
		t.Fatalf("input of b2 reached a1 (%d)", n)
	}

	// ending a1 does not hurt b2
	if code, m, _ := e.call("DELETE", "/v1/sessions/a1", nil, testSecret); code != 200 || m["ok"] != true {
		t.Fatalf("delete a1: %d %v", code, m)
	}
	if n := e.srv.muxes.open(); n != 1 {
		t.Fatalf("socket closed with b2 still running (%d)", n)
	}
	before := pb.vPackets.Load()
	waitFor(t, "b2 keeps streaming", 8*time.Second, func() bool { return pb.vPackets.Load() > before+30 })
	pb.sendInput(1 << 7)
	waitFor(t, "b2 input after a1 ended", 3*time.Second, func() bool { return statI(t, e, "b2", "inputApplied") >= 2 })

	// a freed slot is usable again, sharing the same socket
	e.create("d4")
	pd := newTestPeer(t)
	defer pd.pc.Close()
	pd.negotiate(t, e, "d4")
	waitFor(t, "video on d4", 10*time.Second, func() bool { return pd.vPackets.Load() > 30 })
	if n := e.srv.muxes.open(); n != 1 {
		t.Fatalf("sockets open = %d", n)
	}
	e.call("DELETE", "/v1/sessions/d4", nil, testSecret)
	e.call("DELETE", "/v1/sessions/b2", nil, testSecret)
	if n := e.srv.muxes.open(); n != 0 {
		t.Fatalf("socket leaked after the last session (%d)", n)
	}
}
