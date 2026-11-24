// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type sinkWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *sinkWriter) Close() error { return nil }
func (w *sinkWriter) take() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := append([]byte(nil), w.buf.Bytes()...)
	w.buf.Reset()
	return b
}

var _ io.WriteCloser = (*sinkWriter)(nil)

func TestAdvertiseIpsValidation(t *testing.T) {
	e := newEnv(t, nil)
	for _, bad := range []any{[]string{"a.b.c.d"}, []string{"127.0.0.1"}, []string{"0.0.0.0"}, []string{"169.254.1.1"}, []string{"224.0.0.1"}, []string{"::1"},
		[]string{"fe80::1"}, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, []string{"10.0.0.1/24"}, "10.0.0.1", []int{1}} {
		b := e.createBody("adv")
		b["advertiseIps"] = bad
		if code, m, raw := e.call("POST", "/v1/sessions", b, testSecret); code != 400 || m["error"] != "bad_request" {
			t.Fatalf("%v: %d %s", bad, code, raw)
		}
	}
	if ips, ok := normalizeAdvertise([]string{"10.0.0.5", "10.0.0.5"}); !ok || len(ips) != 1 {
		t.Fatalf("dedupe: %v %v", ips, ok)
	}
	if ips, ok := normalizeAdvertise(nil); !ok || len(ips) != 0 {
		t.Fatal("absent must be fine")
	}
}

func TestAdvertiseIpsCandidates(t *testing.T) {
	e := newEnv(t, nil)
	b := e.createBody("adv")
	b["advertiseIps"] = []string{"10.9.8.7", "192.168.77.5"}
	if code, _, raw := e.call("POST", "/v1/sessions", b, testSecret); code != 201 {
		t.Fatalf("%d %s", code, raw)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	p.negotiate(t, e, "adv") // connects through 127.0.0.1 (iceHost): the socket stays bound there
	for _, ip := range []string{"127.0.0.1", "10.9.8.7", "192.168.77.5"} {
		if !strings.Contains(p.answer, " "+ip+" ") {
			t.Fatalf("answer without a host candidate for %s:\n%s", ip, p.answer)
		}
	}
	// absent field = only iceHost (the behaviour before)
	e2 := newEnv(t, nil)
	e2.create("plain")
	p2 := newTestPeer(t)
	defer p2.pc.Close()
	p2.negotiate(t, e2, "plain")
	if strings.Contains(p2.answer, "10.9.8.7") || !strings.Contains(p2.answer, " 127.0.0.1 ") {
		t.Fatalf("plain session candidates:\n%s", p2.answer)
	}
}

func TestPeersContract(t *testing.T) {
	e := newEnv(t, nil)
	_, h, raw := e.call("GET", "/v1/health", nil, testSecret)
	mp, _ := h["multiplayer"].(map[string]any)
	if mp["snes"] != float64(2) || len(mp) != 1 || h["maxPeers"] != float64(2) {
		t.Fatalf("health multiplayer: %s", raw)
	}
	if code, _, _ := e.call("POST", "/v1/sessions/nope/peers", map[string]any{"slot": 0}, testSecret); code != 404 {
		t.Fatal(code)
	}
	if code, _, _ := e.call("POST", "/v1/sessions/nope/peers", map[string]any{"slot": 0}, ""); code != 401 {
		t.Fatal(code)
	}
	e.create("mp")
	for _, body := range []any{map[string]any{}, map[string]any{"slot": -1}, map[string]any{"slot": 2}, map[string]any{"slot": 9}, map[string]any{"slot": "1"}} {
		if code, m, raw := e.call("POST", "/v1/sessions/mp/peers", body, testSecret); code != 400 || m["error"] != "bad_request" {
			t.Fatalf("%v: %d %s", body, code, raw)
		}
	}
	code, m0, raw := e.call("POST", "/v1/sessions/mp/peers", map[string]any{"slot": 0}, testSecret)
	if code != 201 || m0["peerId"] == nil || m0["peerId"] == "" {
		t.Fatalf("slot 0: %d %s", code, raw)
	}
	if code, m, _ := e.call("POST", "/v1/sessions/mp/peers", map[string]any{"slot": 0}, testSecret); code != 409 || m["error"] != "slot_taken" {
		t.Fatalf("slot_taken: %d %v", code, m)
	}
	code, m1, _ := e.call("POST", "/v1/sessions/mp/peers", map[string]any{"slot": 1}, testSecret)
	if code != 201 {
		t.Fatalf("slot 1: %d", code)
	}
	if code, m, _ := e.call("POST", "/v1/sessions/mp/peers", map[string]any{"slot": 1}, testSecret); code != 409 || m["error"] != "room_full" {
		t.Fatalf("room_full: %d %v", code, m)
	}
	_, st, raw := e.call("GET", "/v1/sessions/mp", nil, testSecret)
	ps, _ := st["peers"].([]any)
	if len(ps) != 2 {
		t.Fatalf("peers: %s", raw)
	}
	p1 := ps[1].(map[string]any)
	if p1["peerId"] != m1["peerId"] || p1["slot"] != float64(1) || p1["state"] != "waiting_signal" {
		t.Fatalf("peer view: %s", raw)
	}
	// signaling names a peer that does not exist
	if code, m, _ := e.call("POST", "/v1/sessions/mp/signal", map[string]any{"type": "offer", "sdp": "x", "peerId": "pzzz"}, testSecret); code != 404 || m["error"] != "peer_not_found" {
		t.Fatalf("unknown peer: %d %v", code, m)
	}
	if code, _, _ := e.call("DELETE", "/v1/sessions/mp/peers/pzzz", nil, testSecret); code != 404 {
		t.Fatal(code)
	}
	// deleting slot 0 does NOT end the session; the slot becomes free again
	if code, _, _ := e.call("DELETE", "/v1/sessions/mp/peers/"+m0["peerId"].(string), nil, testSecret); code != 200 {
		t.Fatal(code)
	}
	if code, _, _ := e.call("GET", "/v1/sessions/mp", nil, testSecret); code != 200 {
		t.Fatal("session ended with the owner's peer")
	}
	if code, _, _ := e.call("POST", "/v1/sessions/mp/peers", map[string]any{"slot": 0}, testSecret); code != 201 {
		t.Fatal(code)
	}
}

func TestPeersNotMultiplayer(t *testing.T) {
	s := &session{srv: &server{cfg: config{}}, req: createReq{Platform: "n64"}}
	if id, code := s.addPeer(1); id != "" || code != "not_multiplayer" {
		t.Fatal(id, code)
	}
	if id, code := s.addPeer(0); id == "" || code != "" {
		t.Fatal(id, code)
	}
	if _, code := s.addPeer(0); code != "room_full" {
		t.Fatal(code)
	}
	// MaxPeers=1 turns the SNES single-player too
	s2 := &session{srv: &server{cfg: config{MaxPeers: 1}}, req: createReq{Platform: "snes"}}
	if _, code := s2.addPeer(0); code != "" {
		t.Fatal(code)
	}
	if _, code := s2.addPeer(1); code != "room_full" {
		t.Fatal(code)
	}
	if (&server{cfg: config{MaxPeers: 9}}).cfg.maxPeers() != 4 || (config{}).maxPeers() != 2 {
		t.Fatal("maxPeers clamp")
	}
}

func TestInputPortOverwrite(t *testing.T) {
	w := &sinkWriter{}
	s := &session{srv: &server{cfg: config{}}, req: createReq{Platform: "snes"}, stdin: w, inputWrite: newRing(8)}
	pkt := func(seq uint16, buttons uint32, port uint8) []byte {
		return encodeInput(inputPacket{seq: seq, buttons: buttons, port: port})
	}
	a := uint32(1 << 4) // a (south) -> RetroPad bit 0
	// the peer of slot 1 claims port 0: it ends up on port 1
	s.onInputFrom(1, pkt(1, a, 0))
	if got := w.take(); !bytes.Equal(got, []byte{1, 1, 0, 1}) {
		t.Fatalf("slot 1 claiming port 0: %v", got)
	}
	// the owner claiming port 3 stays on port 0
	s.onInputFrom(0, pkt(1, a, 3)) // same seq as slot 1: the counters are per peer
	if got := w.take(); !bytes.Equal(got, []byte{1, 1, 0, 0}) {
		t.Fatalf("slot 0 claiming port 3: %v", got)
	}
	// anti-repetition per slot: an old seq of slot 1 is dropped, slot 0 is not affected
	s.onInputFrom(1, pkt(0, 0, 1))
	if len(w.take()) != 0 || s.inOld.Load() != 1 {
		t.Fatal("old seq must be dropped")
	}
	// releasing slot 1 leaves slot 0 pressed
	s.releaseInputSlot(1)
	if got := w.take(); !bytes.Equal(got, []byte{1, 0, 0, 1}) {
		t.Fatalf("release slot 1: %v", got)
	}
	if s.masks[0] == 0 || s.masks[1] != 0 {
		t.Fatal("masks", s.masks)
	}
	// garbage
	s.onInputFrom(1, []byte{1, 2, 3})
	if s.inBad.Load() != 1 {
		t.Fatal("short packet must count as bad")
	}
}

func gotVideo(p *testPeer, n int64) func() bool { return func() bool { return p.vPackets.Load() > n } }

func TestTwoPeersFanOut(t *testing.T) {
	e := newEnv(t, nil)
	e.create("mp")
	idOf := func(slot int) string {
		_, m, raw := e.call("POST", "/v1/sessions/mp/peers", map[string]any{"slot": slot}, testSecret)
		id, _ := m["peerId"].(string)
		if id == "" {
			t.Fatalf("slot %d: %s", slot, raw)
		}
		return id
	}
	p0, p1 := newTestPeer(t), newTestPeer(t)
	defer p0.pc.Close()
	defer p1.pc.Close()
	p0.peerID = idOf(0)
	p0.negotiate(t, e, "mp")
	waitFor(t, "peer 0 video", 10*time.Second, gotVideo(p0, 30))
	p1.peerID = idOf(1)
	p1.negotiate(t, e, "mp")
	waitFor(t, "peer 1 video", 10*time.Second, gotVideo(p1, 30))
	waitFor(t, "peer 1 IDR", 5*time.Second, func() bool { return p1.gotIDR.Load() }) // a connecting peer gets a keyframe
	waitFor(t, "peer 0 audio", 5*time.Second, func() bool { return p0.aPackets.Load() > 5 })
	waitFor(t, "peer 1 audio", 5*time.Second, func() bool { return p1.aPackets.Load() > 5 })
	_, st, raw := e.call("GET", "/v1/sessions/mp", nil, testSecret)
	ps, _ := st["peers"].([]any)
	if st["state"] != "playing" || len(ps) != 2 || ps[0].(map[string]any)["state"] != "playing" || ps[1].(map[string]any)["state"] != "playing" {
		t.Fatalf("state: %s", raw)
	}
	// both inputs reach the child, each on its own port
	p1.sendInputPort(1<<4, 0)
	waitFor(t, "input applied", 3*time.Second, func() bool { return e.srv.get("mp").inApplied.Load() >= 1 })
	e.srv.get("mp").imu.Lock()
	m0, m1 := e.srv.get("mp").masks[0], e.srv.get("mp").masks[1]
	e.srv.get("mp").imu.Unlock()
	if m0 != 0 || m1 == 0 {
		t.Fatalf("peer of slot 1 sending port 0: masks %v %v", m0, m1)
	}
	// the second peer leaves: the first keeps playing and the session lives
	before := p0.vPackets.Load()
	if code, _, _ := e.call("DELETE", "/v1/sessions/mp/peers/"+p1.peerID, nil, testSecret); code != 200 {
		t.Fatal(code)
	}
	waitFor(t, "peer 0 still gets video", 5*time.Second, gotVideo(p0, before+30))
	_, st, raw = e.call("GET", "/v1/sessions/mp", nil, testSecret)
	if st["state"] != "playing" || len(st["peers"].([]any)) != 1 {
		t.Fatalf("after peer 1 left: %s", raw)
	}
	e.srv.get("mp").imu.Lock()
	m1 = e.srv.get("mp").masks[1]
	e.srv.get("mp").imu.Unlock()
	if m1 != 0 {
		t.Fatal("a peer that left must release its buttons")
	}
	// the last peer leaves: nobody is connected (the session ends later, by the grace period)
	if code, _, _ := e.call("DELETE", "/v1/sessions/mp/peers/"+p0.peerID, nil, testSecret); code != 200 {
		t.Fatal(code)
	}
	waitFor(t, "disconnected", 3*time.Second, func() bool { return e.srv.get("mp").State() == "disconnected" })
}
