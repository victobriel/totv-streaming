// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// testPeer is a minimal WebRTC client playing the role of the Fire TV app: asks for video+audio, creates the
// "input" DataChannel (unordered, no retransmits) and records what arrives.
type testPeer struct {
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	extID     atomic.Int32 // negotiated playout-delay id
	vPackets  atomic.Int64
	aPackets  atomic.Int64
	pdOK      atomic.Int64 // video packets carrying playout-delay 0/0
	pdBad     atomic.Int64
	onVideo   func(*rtp.Packet)
	dcOpen    chan struct{}
	connected chan struct{}
	seq       uint16
	mu        sync.Mutex
	peerID    string // multi-peer: the peer this client signals as ("" = the old single-player flow, slot 0)
	answer    string // the SDP answer received
	gotIDR    atomic.Bool
}

func newTestPeer(t *testing.T) *testPeer {
	m := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp, RTCPFeedback: fb}, PayloadType: 102}, webrtc.RTPCodecTypeVideo)
	m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, PayloadType: 111}, webrtc.RTPCodecTypeAudio)
	m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: playoutURI}, webrtc.RTPCodecTypeVideo)
	ir := &interceptor.Registry{}
	webrtc.RegisterDefaultInterceptors(m, ir)
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	p := &testPeer{pc: pc, dcOpen: make(chan struct{}), connected: make(chan struct{})}
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	ordered, rtx := false, uint16(0)
	p.dc, err = pc.CreateDataChannel("input", &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &rtx})
	if err != nil {
		t.Fatal(err)
	}
	p.dc.OnOpen(func() { close(p.dcOpen) })
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			select {
			case <-p.connected:
			default:
				close(p.connected)
			}
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for _, e := range tr.Codec().RTPCodecCapability.RTCPFeedback {
			_ = e
		}
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			if tr.Kind() == webrtc.RTPCodecTypeAudio {
				p.aPackets.Add(1)
				continue
			}
			p.vPackets.Add(1)
			if n := len(pkt.Payload); n > 0 && (pkt.Payload[0]&0x1f == 24 || pkt.Payload[0]&0x1f == 7) { // STAP-A / SPS: the encoder only sends them before an IDR
				p.gotIDR.Store(true)
			}
			if id := p.extID.Load(); id > 0 {
				if ext := pkt.Header.GetExtension(uint8(id)); bytes.Equal(ext, []byte{0, 0, 0}) {
					p.pdOK.Add(1)
				} else {
					p.pdBad.Add(1)
				}
			}
			if p.onVideo != nil {
				p.onVideo(pkt)
			}
		}
	})
	return p
}

// negotiate does the whole signaling through the control API (like the Totv would relay it).
func (p *testPeer) negotiate(t *testing.T, e *testEnv, id string) {
	t.Helper()
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	p.pc.SetLocalDescription(offer)
	<-gathered
	for _, ex := range p.pc.LocalDescription().SDP {
		_ = ex
	}
	// the extension id the browser/app would use for playout-delay
	p.extID.Store(findExtID(p.pc.LocalDescription().SDP, playoutURI))
	code, m, raw := e.call("POST", "/v1/sessions/"+id+"/signal", map[string]string{"type": "offer", "sdp": p.pc.LocalDescription().SDP, "peerId": p.peerID}, testSecret)
	if code != 200 || m["type"] != "answer" {
		t.Fatalf("signal: %d %s", code, raw)
	}
	p.answer, _ = m["sdp"].(string)
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m["sdp"].(string)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer did not connect")
	}
	select {
	case <-p.dcOpen:
	case <-time.After(10 * time.Second):
		t.Fatal("input channel did not open")
	}
}

func (p *testPeer) sendInput(buttons uint32) time.Time { return p.sendInputPort(buttons, 0) }

func (p *testPeer) sendInputPort(buttons uint32, port uint8) time.Time {
	p.mu.Lock()
	p.seq++
	b := encodeInput(inputPacket{seq: p.seq, buttons: buttons, port: port})
	p.mu.Unlock()
	t := time.Now()
	_ = p.dc.Send(b)
	return t
}

func findExtID(sdp, uri string) int32 {
	for _, line := range bytes.Split([]byte(sdp), []byte("\n")) {
		var id int32
		l := string(bytes.TrimSpace(line))
		if len(l) > 9 && l[:9] == "a=extmap:" && bytes.Contains([]byte(l), []byte(uri)) {
			for _, c := range l[9:] {
				if c < '0' || c > '9' {
					break
				}
				id = id*10 + int32(c-'0')
			}
			return id
		}
	}
	return 0
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

var _ = context.Background
var _ = json.Marshal
