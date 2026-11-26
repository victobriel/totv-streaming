// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// peer is one WebRTC client of a session, bound to a player slot (0 = the owner). All peers share ONE encoder, ONE packetizer and the same
// two local tracks (video, audio): the pion tracks write to every PeerConnection they are added to.
type peer struct {
	id             string
	slot           int
	pc             *webrtc.PeerConnection // nil until its offer arrives
	state          string                 // waiting_signal | connecting | playing | disconnected (rtcState.mu)
	created        time.Time
	disconnectedAt time.Time
}

type rtcState struct {
	mu        sync.Mutex
	peers     map[string]*peer
	inited    bool
	api       *webrtc.API
	shared    *sharedMux // the process-wide mux (udpmux.go), released in closeRTC
	vtrack    *playoutTrack
	atrack    *webrtc.TrackLocalStaticSample
	extID     atomic.Int32
	streaming atomic.Bool // at least one peer is connected
	forceIDR  atomic.Bool
	pk        rtp.Packetizer
}

// playersFor is how many players the platform of a session can have (2 for the SNES in this release; no multitap).
func playersFor(platform string) int {
	if platform == "snes" {
		return 2
	}
	return 1
}

func (c config) maxPeers() int {
	switch {
	case c.MaxPeers <= 0:
		return 2
	case c.MaxPeers > 4:
		return 4
	}
	return c.MaxPeers
}

// slotsFor is the number of slots a session of this platform offers: the platform's players capped by TOTV_STREAM_MAX_PEERS.
func (s *session) slotsFor() int {
	n := playersFor(s.req.Platform)
	if m := s.srv.cfg.maxPeers(); m < n {
		n = m
	}
	return n
}

func newPeerID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "p" + hex.EncodeToString(b)
}

// addPeer reserves a slot. Codes: "" ok, "bad_request" (slot out of range), "not_multiplayer", "room_full", "slot_taken".
func (s *session) addPeer(slot int) (string, string) {
	if slot < 0 || slot > 3 {
		return "", "bad_request"
	}
	if slot > 0 && playersFor(s.req.Platform) < 2 {
		return "", "not_multiplayer"
	}
	r := &s.rtc
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.peers) >= s.slotsFor() {
		return "", "room_full"
	}
	if slot >= s.slotsFor() {
		return "", "bad_request"
	}
	for _, p := range r.peers {
		if p.slot == slot {
			return "", "slot_taken"
		}
	}
	return s.newPeerLocked(slot).id, ""
}

func (s *session) newPeerLocked(slot int) *peer {
	r := &s.rtc
	if r.peers == nil {
		r.peers = map[string]*peer{}
	}
	p := &peer{id: newPeerID(), slot: slot, state: "waiting_signal", created: time.Now()}
	r.peers[p.id] = p
	s.resetSeq(slot)
	return p
}

// findPeerLocked: "" = the peer of slot 0 (compatibility: the single-player flow never names a peer).
func (r *rtcState) findPeerLocked(id string) *peer {
	if id != "" {
		return r.peers[id]
	}
	for _, p := range r.peers {
		if p.slot == 0 {
			return p
		}
	}
	return nil
}

// removePeer drops one peer only: its slot is free again and its input released. The session is NOT ended here (see monitor).
func (s *session) removePeer(id string) bool {
	r := &s.rtc
	r.mu.Lock()
	p := r.peers[id]
	if p == nil {
		r.mu.Unlock()
		return false
	}
	delete(r.peers, id)
	s.refreshLocked()
	r.mu.Unlock()
	s.releaseInputSlot(p.slot)
	if p.pc != nil {
		_ = p.pc.Close()
	}
	return true
}

// peersView is the `peers` of GET /v1/sessions/{id}.
func (s *session) peersView() []map[string]any {
	r := &s.rtc
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, map[string]any{"peerId": p.id, "slot": p.slot, "state": p.state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["slot"].(int) < out[j]["slot"].(int) })
	return out
}

// refreshLocked derives the session state from its peers (rtcState.mu held): streaming = at least one peer connected.
func (s *session) refreshLocked() {
	r := &s.rtc
	n := 0
	for _, p := range r.peers {
		if p.state == "playing" {
			n++
		}
	}
	r.streaming.Store(n > 0)
	s.mu.Lock()
	if n > 0 {
		if s.connectedAt.IsZero() {
			s.connectedAt = time.Now()
		}
		s.disconnectedAt = time.Time{}
	} else {
		if !s.connectedAt.IsZero() {
			s.playedBefore += time.Since(s.connectedAt)
			s.connectedAt = time.Time{}
		}
		if s.disconnectedAt.IsZero() {
			s.disconnectedAt = time.Now()
		}
	}
	s.mu.Unlock()
	if n > 0 {
		s.setState("playing")
	} else {
		s.setState("disconnected")
	}
}

func (s *session) onPeerState(p *peer, st webrtc.PeerConnectionState) {
	r := &s.rtc
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.peers[p.id] != p {
		return
	}
	switch st {
	case webrtc.PeerConnectionStateConnected:
		p.state, p.disconnectedAt = "playing", time.Time{}
		r.forceIDR.Store(true) // the new peer (and nobody else's stream) needs an IDR to start decoding
		s.refreshLocked()
		log.Printf("session %s: peer %s (slot %d) connected, playout-delay ext id=%d", s.id, p.id, p.slot, r.extID.Load())
	case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		p.state = "disconnected"
		if p.disconnectedAt.IsZero() {
			p.disconnectedAt = time.Now()
		}
		s.releaseInputSlot(p.slot)
		s.refreshLocked()
	}
}

// reapPeers (monitor tick) applies the per-peer timers: a slot that never got its offer in NoSignalTimeout, or whose peer has been gone for
// DisconnectGrace, is freed. The session itself ends only when no peer is connected for DisconnectGrace (monitor).
func (s *session) reapPeers() {
	r := &s.rtc
	cfg := s.srv.cfg
	var dead []string
	r.mu.Lock()
	for id, p := range r.peers {
		if (p.pc == nil && time.Since(p.created) > cfg.NoSignalTimeout) || (p.state == "disconnected" && !p.disconnectedAt.IsZero() && time.Since(p.disconnectedAt) > cfg.DisconnectGrace) {
			dead = append(dead, id)
		}
	}
	r.mu.Unlock()
	for _, id := range dead {
		log.Printf("session %s: peer %s freed by timeout", s.id, id)
		s.removePeer(id)
	}
}

func newAPI(mux ice.UDPMux, ips []string, fmtp string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp, RTCPFeedback: fb}, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: playoutURI}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetNAT1To1IPs(ips, webrtc.ICECandidateTypeHost) // candidates ONLY with iceHost (+ advertiseIps); the socket stays bound to iceHost
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// candidateIPs: advertiseIps ∪ {iceIP}, iceIP first, no duplicates.
func (s *session) candidateIPs() []string {
	out := []string{s.iceIP}
	for _, ip := range s.req.AdvertiseIps {
		dup := false
		for _, o := range out {
			dup = dup || o == ip
		}
		if !dup {
			out = append(out, ip)
		}
	}
	return out
}

// initRTCLocked creates what all peers share: the UDP socket and mux, the API, the two tracks and the packetizer (rtcState.mu held).
func (s *session) initRTCLocked() string {
	r := &s.rtc
	if r.inited {
		return ""
	}
	sm, err := s.srv.muxes.acquire(s.iceIP, s.srv.cfg.UDPPort)
	if err != nil {
		log.Printf("session %s: udp listen failed", s.id)
		return "udp_unavailable"
	}
	r.shared = sm
	fmtp := h264FmtpFor(h264Level(s.prof.W, s.prof.H, s.encoderFPS()))
	api, err := newAPI(sm.mux, s.candidateIPs(), fmtp)
	if err != nil {
		return "internal"
	}
	r.api = api
	vt, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp}, "video", "totv")
	if err != nil {
		return "internal"
	}
	r.vtrack = &playoutTrack{TrackLocalStaticRTP: vt, extID: &r.extID}
	r.atrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "totv")
	if err != nil {
		return "internal"
	}
	r.pk = rtp.NewPacketizer(1200, 102, uint32(time.Now().UnixNano()), &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	r.inited = true
	return ""
}

// handleOffer negotiates one peer. peerID "" = the peer of slot 0 (created on the spot when the Totv never called POST .../peers: the old
// single-player flow). Codes: "" ok, peer_not_found, already_negotiated, udp_unavailable, internal, bad_offer.
func (s *session) handleOffer(ctx context.Context, peerID, sdp string) (string, string) {
	r := &s.rtc
	r.mu.Lock()
	p := r.findPeerLocked(peerID)
	if p == nil {
		if peerID != "" {
			r.mu.Unlock()
			return "", "peer_not_found"
		}
		p = s.newPeerLocked(0)
	}
	if p.pc != nil {
		r.mu.Unlock()
		return "", "already_negotiated"
	}
	if code := s.initRTCLocked(); code != "" {
		r.mu.Unlock()
		return "", code
	}
	pc, err := r.api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{}}) // no STUN, no TURN
	if err != nil {
		r.mu.Unlock()
		return "", "internal"
	}
	p.pc, p.state = pc, "connecting"
	vt, at := r.vtrack, r.atrack
	r.mu.Unlock()

	fail := func(code string) (string, string) { // the slot stays reserved; the Totv may DELETE the peer or retry after DELETE + POST
		_ = pc.Close()
		r.mu.Lock()
		p.pc, p.state = nil, "waiting_signal"
		r.mu.Unlock()
		return "", code
	}
	for _, t := range []webrtc.TrackLocal{vt, at} {
		sender, err := pc.AddTrack(t)
		if err != nil {
			return fail("internal")
		}
		go s.readRTCP(sender)
	}
	slot := p.slot
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			_ = dc.Close()
			return
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) { s.onInputFrom(slot, m.Data) })
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) { s.onPeerState(p, st) })
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return fail("bad_offer")
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail("bad_offer")
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		return fail("bad_offer")
	}
	select {
	case <-gathered:
	case <-time.After(4 * time.Second):
	case <-ctx.Done():
	}
	if !r.streaming.Load() {
		s.setState("connecting")
	}
	return announceLevel(pc.LocalDescription().SDP, h264Level(s.prof.W, s.prof.H, s.encoderFPS())), ""
}

func (s *session) handleCandidate(peerID string, raw json.RawMessage) string {
	r := &s.rtc
	r.mu.Lock()
	p := r.findPeerLocked(peerID)
	var pc *webrtc.PeerConnection
	if p != nil {
		pc = p.pc
	}
	r.mu.Unlock()
	if p == nil && peerID != "" {
		return "peer_not_found"
	}
	if pc == nil {
		return "no_offer"
	}
	if len(raw) > 4096 {
		return "bad_request"
	}
	var c struct {
		Candidate     string  `json:"candidate"`
		SDPMid        *string `json:"sdpMid"`
		SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
	}
	if len(raw) > 0 && raw[0] == '"' {
		if json.Unmarshal(raw, &c.Candidate) != nil {
			return "bad_request"
		}
	} else if json.Unmarshal(raw, &c) != nil {
		return "bad_request"
	}
	if c.Candidate == "" {
		return "bad_request"
	}
	if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex}); err != nil {
		return "bad_candidate"
	}
	return ""
}

// readRTCP: a PLI/FIR from ANY peer asks the shared encoder for an IDR.
func (s *session) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.rtc.forceIDR.Store(true)
			}
		}
	}
}

func (s *session) closeRTC() {
	r := &s.rtc
	r.mu.Lock()
	var pcs []*webrtc.PeerConnection
	for _, p := range r.peers {
		if p.pc != nil {
			pcs = append(pcs, p.pc)
		}
	}
	sm := r.shared
	r.shared = nil
	r.mu.Unlock()
	for _, pc := range pcs {
		_ = pc.Close()
	}
	s.srv.muxes.release(sm) // the socket stays open for the other sessions
}
