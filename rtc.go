// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const playoutURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

// h264Fmtp is the SDP of the old fixed 640x480 stream (level 3.1); a session announces the level of its own picture (h264FmtpFor in codec.go).
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

// encoderFPS is the frame rate x264 assumes: the core's own (50 for a PAL N64, 59.94 NTSC) once its hello arrived, 60 before.
func (s *session) encoderFPS() float64 {
	if f := s.fpsNow(); f >= 10 && f <= 144 {
		return f
	}
	return 60
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
			c := s.srv.cfg
			if enc, err = newVideoEncoder(encoderParams{W: s.prof.W, H: s.prof.H, FPS: s.encoderFPS(), CRF: c.CRF, MaxKbps: s.prof.Kbps, BufFrames: 6,
				Threads: c.X264Threads, Preset: c.X264Preset}); err != nil {
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
