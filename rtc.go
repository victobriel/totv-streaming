// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const playoutURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"
const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// playoutTrack is a video track that learns the negotiated id of the playout-delay extension.
type playoutTrack struct {
	*webrtc.TrackLocalStaticRTP
	extID *atomic.Int32
}

func (t *playoutTrack) Bind(c webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	for _, e := range c.HeaderExtensions() {
		if e.URI == playoutURI {
			t.extID.Store(int32(e.ID))
		}
	}
	return t.TrackLocalStaticRTP.Bind(c)
}

type rtcState struct {
	mu        sync.Mutex
	pc        *webrtc.PeerConnection
	mux       ice.UDPMux
	conn      net.PacketConn
	vtrack    *playoutTrack
	atrack    *webrtc.TrackLocalStaticSample
	extID     atomic.Int32
	streaming atomic.Bool
	forceIDR  atomic.Bool
	pk        rtp.Packetizer
}

func newAPI(mux ice.UDPMux, ip string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp, RTCPFeedback: fb}, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
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
	se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost) // candidates ONLY with iceHost
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

func (s *session) handleOffer(ctx context.Context, sdp string) (string, string) {
	r := &s.rtc
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pc != nil {
		return "", "already_negotiated"
	}
	conn, err := net.ListenPacket("udp4", net.JoinHostPort(s.iceIP, strconv.Itoa(s.srv.cfg.UDPPort)))
	if err != nil {
		log.Printf("session %s: udp listen failed", s.id)
		return "", "udp_unavailable"
	}
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn})
	r.conn, r.mux = conn, mux
	api, err := newAPI(mux, s.iceIP)
	if err != nil {
		return "", "internal"
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{}}) // no STUN, no TURN
	if err != nil {
		return "", "internal"
	}
	r.pc = pc

	r.vtrack = &playoutTrack{TrackLocalStaticRTP: nil, extID: &r.extID}
	vt, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp}, "video", "totv")
	if err != nil {
		return "", "internal"
	}
	r.vtrack.TrackLocalStaticRTP = vt
	r.atrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "totv")
	if err != nil {
		return "", "internal"
	}
	r.pk = rtp.NewPacketizer(1200, 102, uint32(time.Now().UnixNano()), &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	for _, t := range []webrtc.TrackLocal{r.vtrack, r.atrack} {
		sender, err := pc.AddTrack(t)
		if err != nil {
			return "", "internal"
		}
		go s.readRTCP(sender)
	}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			_ = dc.Close()
			return
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) { s.onInput(m.Data) })
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.mu.Lock()
			s.connectedAt = time.Now()
			s.disconnectedAt = time.Time{}
			s.mu.Unlock()
			r.forceIDR.Store(true)
			r.streaming.Store(true)
			s.setState("playing")
			log.Printf("session %s: peer connected, playout-delay ext id=%d", s.id, r.extID.Load())
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			r.streaming.Store(false)
			s.releaseInput()
			s.mu.Lock()
			if !s.connectedAt.IsZero() {
				s.playedBefore += time.Since(s.connectedAt)
				s.connectedAt = time.Time{}
			}
			if s.disconnectedAt.IsZero() {
				s.disconnectedAt = time.Now()
			}
			s.mu.Unlock()
			s.setState("disconnected")
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", "bad_offer"
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", "bad_offer"
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		return "", "bad_offer"
	}
	select {
	case <-gathered:
	case <-time.After(4 * time.Second):
	case <-ctx.Done():
	}
	s.setState("connecting")
	return pc.LocalDescription().SDP, ""
}

func (s *session) handleCandidate(raw json.RawMessage) string {
	r := &s.rtc
	r.mu.Lock()
	pc := r.pc
	r.mu.Unlock()
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
	defer r.mu.Unlock()
	if r.pc != nil {
		_ = r.pc.Close()
	}
	if r.mux != nil {
		_ = r.mux.Close()
	}
	if r.conn != nil {
		_ = r.conn.Close()
	}
}

// videoLoop encodes the freshest frame (x264 baseline, no B-frames) and sends it with playout-delay 0/0.
func (s *session) videoLoop() {
	var enc *videoEncoder
	defer func() {
		if enc != nil {
			enc.Close()
		}
	}()
	var lastAU time.Time
	for {
		var f frame
		select {
		case f = <-s.frames:
		case <-s.quit:
			return
		}
		if !s.rtc.streaming.Load() {
			s.pool.Put(f.buf)
			if enc != nil { // reconnect starts from a fresh IDR
				enc.Close()
				enc = nil
			}
			continue
		}
		if enc == nil {
			var err error
			ow, oh := s.srv.size(s.ci)
			if enc, err = newVideoEncoder(ow, oh, 60, s.srv.cfg.Kbps, 60); err != nil {
				log.Printf("session %s: encoder: %v", s.id, err)
				return
			}
			lastAU = time.Time{}
		}
		t0 := time.Now()
		au, err := enc.Encode(f.buf, s.rtc.forceIDR.Swap(false))
		s.pool.Put(f.buf)
		s.encodeMs.add(time.Since(t0))
		if err != nil || len(au) == 0 {
			continue
		}
		s.vEncoded.Add(1)
		now := time.Now()
		samples := uint32(1500)
		if !lastAU.IsZero() {
			samples = uint32(now.Sub(lastAU).Seconds() * 90000)
		}
		lastAU = now
		ext := s.rtc.extID.Load()
		for _, p := range s.rtc.pk.Packetize(au, samples) {
			if ext > 0 {
				// playout-delay: min 0, max 0 (12 bits each, units of 10 ms)
				if err := p.Header.SetExtension(uint8(ext), []byte{0, 0, 0}); err == nil {
					s.pdSent.Add(1)
				}
			}
			if err := s.rtc.vtrack.WriteRTP(p); err == nil {
				s.bytesOut.Add(int64(p.MarshalSize()))
			}
		}
		s.wireMs.add(time.Since(f.at))
	}
}

// ---- audio: resample the core rate to 48 kHz (linear), Opus 20 ms ----

type resampler struct {
	ratio float64
	in    []int16
	pos   float64
	out   []int16
	enc   *audioEncoder
}

func newResampler() *resampler { return &resampler{ratio: 1} }
func (r *resampler) setRate(hz float64) {
	if hz > 1000 {
		r.ratio = hz / 48000
	}
}

func (s *session) onAudio(r *resampler, pcm []byte) {
	if !s.rtc.streaming.Load() {
		r.in, r.out, r.pos = r.in[:0], r.out[:0], 0
		return
	}
	if r.enc == nil {
		var err error
		if r.enc, err = newAudioEncoder(96); err != nil {
			return
		}
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		r.in = append(r.in, int16(uint16(pcm[i])|uint16(pcm[i+1])<<8))
	}
	frames := len(r.in) / 2
	for int(r.pos)+1 < frames {
		i := int(r.pos)
		fr := r.pos - float64(i)
		for c := 0; c < 2; c++ {
			a, b := float64(r.in[2*i+c]), float64(r.in[2*(i+1)+c])
			r.out = append(r.out, int16(a+(b-a)*fr))
		}
		r.pos += r.ratio
	}
	drop := int(r.pos)
	if drop > frames {
		drop = frames
	}
	r.in = append(r.in[:0], r.in[2*drop:]...)
	r.pos -= float64(drop)
	for len(r.out) >= 1920 {
		pkt, err := r.enc.Encode(r.out[:1920])
		r.out = append(r.out[:0], r.out[1920:]...)
		if err != nil {
			continue
		}
		if err := s.rtc.atrack.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond}); err == nil {
			s.aPackets.Add(1)
		}
	}
}

var _ = errors.New
