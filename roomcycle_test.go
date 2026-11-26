// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"testing"
	"time"
)

// A guest who leaves a room and enters another one (or comes back): the slot is free again at once, the shared UDP mux is neither leaked nor
// closed under the other session, and the host keeps playing. Several rounds, so a leaked reference or an unreleased slot shows up.
func TestGuestLeaveAndReenterAcrossSessions(t *testing.T) {
	e := newEnv(t, func(c *config) { c.MaxSessions = 2 })
	e.create("ra")
	e.create("rb")
	addPeer := func(sid string, slot int) (string, int) {
		code, m, _ := e.call("POST", "/v1/sessions/"+sid+"/peers", map[string]any{"slot": slot}, testSecret)
		id, _ := m["peerId"].(string)
		return id, code
	}
	host := map[string]*testPeer{}
	for _, sid := range []string{"ra", "rb"} {
		p := newTestPeer(t)
		defer p.pc.Close()
		id, code := addPeer(sid, 0)
		if id == "" {
			t.Fatalf("%s host peer: %d", sid, code)
		}
		p.peerID = id
		p.negotiate(t, e, sid)
		waitFor(t, sid+" host video", 10*time.Second, gotVideo(p, 30))
		host[sid] = p
	}
	if n := e.srv.muxes.open(); n != 1 {
		t.Fatalf("sockets open = %d, want 1 shared", n)
	}
	for round := 0; round < 4; round++ {
		sid, other := "ra", "rb"
		if round%2 == 1 {
			sid, other = "rb", "ra"
		}
		g := newTestPeer(t)
		id, code := addPeer(sid, 1)
		if id == "" {
			t.Fatalf("round %d: guest slot 1 of %s refused after the previous guest left: %d", round, sid, code)
		}
		g.peerID = id
		g.negotiate(t, e, sid)
		waitFor(t, "guest video", 10*time.Second, gotVideo(g, 30))
		if code, m, _ := e.call("POST", "/v1/sessions/"+sid+"/peers", map[string]any{"slot": 1}, testSecret); code != 409 || (m["error"] != "slot_taken" && m["error"] != "room_full") {
			t.Fatalf("round %d: a second guest in the taken slot: %d %v", round, code, m)
		}
		before := host[other].vPackets.Load()
		if code, _, _ := e.call("DELETE", "/v1/sessions/"+sid+"/peers/"+g.peerID, nil, testSecret); code != 200 {
			t.Fatalf("round %d: guest leave: %d", round, code)
		}
		g.pc.Close()
		waitFor(t, "the other room keeps playing", 5*time.Second, gotVideo(host[other], before+30))
		if _, st, raw := e.call("GET", "/v1/sessions/"+sid, nil, testSecret); st["state"] != "playing" || len(st["peers"].([]any)) != 1 {
			t.Fatalf("round %d: host of %s after the guest left: %s", round, sid, raw)
		}
		if n := e.srv.muxes.open(); n != 1 {
			t.Fatalf("round %d: sockets open = %d, want 1", round, n)
		}
	}
	// a room closes with a guest inside: the guest's peer goes with it; the other room still takes a new guest, on the same socket
	g := newTestPeer(t)
	defer g.pc.Close()
	id, _ := addPeer("ra", 1)
	g.peerID = id
	g.negotiate(t, e, "ra")
	waitFor(t, "guest in ra", 10*time.Second, gotVideo(g, 30))
	if code, _, _ := e.call("DELETE", "/v1/sessions/ra", nil, testSecret); code != 200 {
		t.Fatalf("stop ra: %d", code)
	}
	if n := e.srv.muxes.open(); n != 1 {
		t.Fatalf("sockets open = %d after one session closed, want 1 (rb still holds it)", n)
	}
	g2 := newTestPeer(t)
	defer g2.pc.Close()
	id, code := addPeer("rb", 1)
	if id == "" {
		t.Fatalf("guest of the closed room cannot enter rb: %d", code)
	}
	g2.peerID = id
	g2.negotiate(t, e, "rb")
	waitFor(t, "guest in rb after ra closed", 10*time.Second, gotVideo(g2, 30))
	if code, _, _ := e.call("DELETE", "/v1/sessions/rb", nil, testSecret); code != 200 {
		t.Fatalf("stop rb: %d", code)
	}
	if n := e.srv.muxes.open(); n != 0 {
		t.Fatalf("sockets open = %d after the last session, want 0", n)
	}
}
