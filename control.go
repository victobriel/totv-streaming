// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type server struct {
	cfg      config
	iso      isolation
	limits   cgroupLimits // pids.max / memory.max of the component's cgroup, read at start
	gpu      gpuInfo      // result of retro-emu --gltest at start
	cg       cgSettings   // per-session cgroup decided at start (cgroup.go)
	cgOK     atomic.Int64 // sessions whose cgroup limits matched when read back from the kernel
	cgBad    atomic.Int64 // sessions that could not be proved to be inside the limits
	mu       sync.Mutex
	sessions map[string]*session
}

func newServer(cfg config, iso isolation) *server {
	srv := &server{cfg: cfg, iso: iso, limits: readCgroupLimits(), cg: probeCgroup(cfg), sessions: map[string]*session{}}
	if srv.cg.l.State == "enforced" { // every session has its own pids.max/memory.max: the component's own cgroup is no longer what bounds a hostile core
		kept := []string{}
		for _, w := range srv.limits.Warnings {
			if !strings.HasPrefix(w, "no pids.max") && !strings.HasPrefix(w, "no memory.max") {
				kept = append(kept, w)
			}
		}
		srv.limits.Warnings = kept
	}
	return srv
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, e string) {
	writeJSON(w, code, map[string]string{"error": e})
}

// authOK compares the bearer with the secret file in constant time (file re-read per call: rotation needs no restart).
func (s *server) authOK(r *http.Request) bool {
	raw, err := os.ReadFile(s.cfg.SecretFile)
	secret := strings.TrimSpace(string(raw))
	h := r.Header.Get("Authorization")
	got := strings.TrimPrefix(h, "Bearer ")
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(secret))
	eq := subtle.ConstantTimeCompare(a[:], b[:]) == 1
	return err == nil && secret != "" && strings.HasPrefix(h, "Bearer ") && eq
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/sessions", s.list)
	mux.HandleFunc("POST /v1/sessions", s.create)
	mux.HandleFunc("GET /v1/sessions/{id}", s.status)
	mux.HandleFunc("PATCH /v1/sessions/{id}", s.patch)
	mux.HandleFunc("POST /v1/sessions/{id}/signal", s.signal)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.remove)
	mux.HandleFunc("POST /v1/sessions/{id}/peers", s.addPeer)
	mux.HandleFunc("DELETE /v1/sessions/{id}/peers/{peerId}", s.removePeer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authOK(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		log.Printf("%s %s", r.Method, r.URL.Path) // never the body, the SDP or the token
		mux.ServeHTTP(w, r)
	})
}

func (s *server) availableCores() map[string][]string {
	cores := map[string][]string{}
	files, _ := filepath.Glob(filepath.Join(s.cfg.CoresDir, "*_libretro.so"))
	sort.Strings(files)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), "_libretro.so")
		if ci, ok := s.coreAllowed(name); ok && (!ci.gl || s.gpu.OK) && (!ci.sysAssets || s.assetsOK()) && (!ci.needBIOS || ps2BiosScan(s.cfg.PS2BiosDir).OK) {
			cores[ci.platform] = append(cores[ci.platform], "server_"+name)
		}
	}
	return cores
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	var gpu any
	if s.gpu.OK {
		gpu = map[string]any{"api": "egl+opengl", "renderer": s.gpu.Renderer, "version": s.gpu.Version}
	}
	writeJSON(w, 200, map[string]any{"version": version, "encoder": "x264", "gpu": gpu, "cores": s.availableCores(), "isolation": s.iso, "limits": s.limitsView(), "ps2": s.ps2Health(),
		"multiplayer": s.multiplayerView(), "maxPeers": s.cfg.maxPeers()})
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]string{}
	for id, ss := range s.sessions {
		out = append(out, map[string]string{"id": id, "state": ss.State()})
	}
	writeJSON(w, 200, out)
}

type createReq struct {
	SessionID    string   `json:"sessionId"`
	ItemID       int64    `json:"itemId"`
	MediaID      int64    `json:"mediaId"`
	Platform     string   `json:"platform"`
	Core         string   `json:"core"`
	Token        string   `json:"token"`
	ServerURL    string   `json:"serverUrl"`
	IceHost      string   `json:"iceHost"`
	AdvertiseIps []string `json:"advertiseIps,omitempty"` // 0..2 literal IPv4s added (with iceHost) as host candidates; the socket stays bound to iceHost
	Aspect       string   `json:"aspect,omitempty"`       // "auto" (default) | "4:3" | "16:9" | "fill" | "pixel": how the picture is placed in the fixed 16:9 output (PATCH changes it live)
	Quality      string   `json:"quality,omitempty"`      // "high" | "safe" ("" = the server default, TOTV_STREAM_QUALITY): "safe" is the way to lower a session that reports overBudget
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var coreRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var q createReq
	if json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&q) != nil {
		writeErr(w, 400, "bad_request")
		return
	}
	core := strings.TrimPrefix(q.Core, "server_")
	ci, coreOK := s.coreAllowed(core)
	if !idRe.MatchString(q.SessionID) || q.ItemID <= 0 || q.MediaID <= 0 || (coreOK && q.Platform != ci.platform) || !coreRe.MatchString(core) || (q.Quality != "" && q.Quality != "high" && q.Quality != "safe") || (q.Aspect != "" && aspectCode(q.Aspect) < 0) {
		writeErr(w, 400, "bad_request")
		return
	}
	if !coreOK || (ci.gl && !s.gpu.OK) {
		writeErr(w, 400, "unknown_core")
		return
	}
	if ci.needBIOS {
		if r := ps2BiosScan(s.cfg.PS2BiosDir); !r.OK { // before a byte of the image is downloaded
			writeErr(w, 502, r.Reason)
			return
		}
	}
	if ci.sysAssets && !s.assetsOK() {
		writeErr(w, 502, "system_assets_missing") // the PPSSPP assets are not installed next to the cores (README, "PSP")
		return
	}
	corePath := filepath.Join(s.cfg.CoresDir, core+"_libretro.so")
	if _, err := os.Stat(corePath); err != nil {
		writeErr(w, 400, "unknown_core")
		return
	}
	if !(s.iso.Seccomp && s.iso.Landlock) && !s.cfg.AllowNoIsolation {
		writeErr(w, 503, "isolation_unavailable")
		return
	}
	tc, err := newTotvClient(q.ServerURL, q.Token)
	ip := parseIPv4(q.IceHost)
	adv, advOK := normalizeAdvertise(q.AdvertiseIps)
	q.AdvertiseIps = adv
	if err != nil || ip == "" || !advOK {
		writeErr(w, 400, "bad_request")
		return
	}
	s.mu.Lock()
	if len(s.sessions) >= s.cfg.MaxSessions {
		s.mu.Unlock()
		writeErr(w, 409, "busy")
		return
	}
	if _, dup := s.sessions[q.SessionID]; dup {
		s.mu.Unlock()
		writeErr(w, 409, "exists")
		return
	}
	ss := newSession(s, q, core, corePath, tc, ip)
	s.sessions[q.SessionID] = ss // slot reserved while starting
	s.mu.Unlock()

	wait := 12 * time.Second
	if ci.startWait > 0 {
		wait = ci.startWait // an ISO of 1 GB is downloaded first
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	if code := ss.start(ctx); code != "" {
		ss.teardown()
		s.drop(q.SessionID)
		status := 502
		if code == "isolation_unavailable" {
			status = 503
		}
		if d, _ := ss.failDetail.Load().(string); d != "" {
			writeJSON(w, status, map[string]string{"error": code, "detail": d})
			return
		}
		writeErr(w, status, code)
		return
	}
	go ss.monitor()
	writeJSON(w, 201, map[string]string{"id": q.SessionID})
}

func (s *server) drop(id string) { s.mu.Lock(); delete(s.sessions, id); s.mu.Unlock() }
func (s *server) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	ss := s.get(r.PathValue("id"))
	if ss == nil {
		writeErr(w, 404, "not_found")
		return
	}
	writeJSON(w, 200, ss.Status())
}

// aspectNames are the picture placement modes, in the order of the host's ASP_* constants (emu/host.c); the index is the byte of the kind-4 message.
var aspectNames = [...]string{"auto", "4:3", "16:9", "fill", "pixel"}

// aspectCode is the host's code of a mode name, -1 when it is not one of them.
func aspectCode(a string) int {
	for i, n := range aspectNames {
		if n == a {
			return i
		}
	}
	return -1
}

// patch changes a live session. Today only {"aspect": mode}: the host re-places the picture inside the fixed output (no restart, the encoder keeps its size).
// Any other field, a second JSON value, an empty body or an unknown mode is a 400.
func (s *server) patch(w http.ResponseWriter, r *http.Request) {
	ss := s.get(r.PathValue("id"))
	if ss == nil {
		writeErr(w, 404, "not_found")
		return
	}
	var m struct {
		Aspect *string `json:"aspect"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	dec.DisallowUnknownFields()
	var extra json.RawMessage
	if dec.Decode(&m) != nil || dec.Decode(&extra) != io.EOF || m.Aspect == nil || aspectCode(*m.Aspect) < 0 {
		writeErr(w, 400, "bad_request")
		return
	}
	ss.touch()
	ss.setAspect(*m.Aspect)
	writeJSON(w, 200, map[string]any{"ok": true, "aspect": *m.Aspect})
}

func (s *server) signal(w http.ResponseWriter, r *http.Request) {
	ss := s.get(r.PathValue("id"))
	if ss == nil {
		writeErr(w, 404, "not_found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		writeErr(w, 413, "too_large")
		return
	}
	var m struct {
		PeerID    string          `json:"peerId"`
		Type      string          `json:"type"`
		SDP       string          `json:"sdp"`
		Candidate json.RawMessage `json:"candidate"`
	}
	if json.Unmarshal(body, &m) != nil {
		writeErr(w, 400, "bad_request")
		return
	}
	ss.touch()
	switch {
	case m.Type == "offer" && m.SDP != "":
		ans, code := ss.handleOffer(r.Context(), m.PeerID, m.SDP)
		if code != "" {
			writeErr(w, signalStatus(code), code)
			return
		}
		writeJSON(w, 200, map[string]string{"type": "answer", "sdp": ans})
	case len(m.Candidate) > 0 && string(m.Candidate) != "null":
		if code := ss.handleCandidate(m.PeerID, m.Candidate); code != "" {
			writeErr(w, signalStatus(code), code)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case string(m.Candidate) == "null":
		writeJSON(w, 200, map[string]bool{"ok": true}) // end of candidates: nothing to do
	default:
		writeErr(w, 400, "bad_request")
	}
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ss := s.get(id)
	if ss == nil {
		writeErr(w, 404, "not_found")
		return
	}
	saved, err := ss.stop(true)
	s.drop(id)
	if err != nil {
		writeErr(w, 502, "save_failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "saved": saved})
}

func (s *server) stopAll(ctx context.Context) {
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		all = append(all, ss)
	}
	s.mu.Unlock()
	for _, ss := range all {
		_, _ = ss.stop(true)
	}
}

// assetsOK: the PPSSPP system directory holds what the core needs to start (the PSP's own font files are NOT part of it, see README).
func (s *server) assetsOK() bool {
	st, err := os.Stat(filepath.Join(s.cfg.PSPSystemDir, "PPSSPP", "ppge_atlas.zim"))
	return err == nil && st.Mode().IsRegular()
}

// pspAssetsNoText reports a PSP system directory that has ppge_atlas.zim but not ppge_atlas.meta. The core still starts, but PPGe (the PSP's
// own on-screen dialogs: savedata list, "There is no data.", the button hints) then has no glyph table and draws every dialog WITHOUT text:
// a blank box that looks frozen, closed only by a button the player cannot see (Circle). Returns the missing file's path, "" when complete.
func pspAssetsNoText(systemDir string) string {
	zim := filepath.Join(systemDir, "PPSSPP", "ppge_atlas.zim")
	if st, err := os.Stat(zim); err != nil || !st.Mode().IsRegular() {
		return "" // no assets at all: assetsOK() already says so
	}
	meta := filepath.Join(systemDir, "PPSSPP", "ppge_atlas.meta")
	if st, err := os.Stat(meta); err != nil || !st.Mode().IsRegular() {
		return meta
	}
	return ""
}

// limitsView is `limits` of /v1/health: the component's own cgroup (as before, flattened) plus `session`, the per-session cgroup.
func (s *server) limitsView() any {
	ss := s.cg.l
	ss.Verified, ss.Violation = int(s.cgOK.Load()), int(s.cgBad.Load())
	return struct {
		cgroupLimits
		Session sessionLimits `json:"session"`
	}{s.limits, ss}
}

func signalStatus(code string) int {
	if code == "peer_not_found" {
		return 404
	}
	return 400
}

// normalizeAdvertise validates `advertiseIps`: at most 2 literal IPv4 addresses that can be a host candidate (no loopback, unspecified, multicast,
// link-local or broadcast), duplicates dropped. false = 400 bad_request.
func normalizeAdvertise(in []string) ([]string, bool) {
	if len(in) > 2 {
		return nil, false
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		ip := net.ParseIP(raw)
		if strings.Contains(raw, ":") || ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.Equal(net.IPv4bcast) {
			return nil, false
		}
		v := ip.To4().String()
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out, true
}

// multiplayerView is `multiplayer` of /v1/health: platforms with more than one player and how many (capped by TOTV_STREAM_MAX_PEERS).
func (s *server) multiplayerView() map[string]int {
	out := map[string]int{}
	if n := playersFor("snes"); n > 1 {
		if m := s.cfg.maxPeers(); m < n {
			n = m
		}
		if n > 1 {
			out["snes"] = n
		}
	}
	return out
}

// addPeer: POST /v1/sessions/{id}/peers {slot} -> 201 {peerId}.
func (s *server) addPeer(w http.ResponseWriter, r *http.Request) {
	ss := s.get(r.PathValue("id"))
	if ss == nil {
		writeErr(w, 404, "not_found")
		return
	}
	var m struct {
		Slot *int `json:"slot"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&m) != nil || m.Slot == nil {
		writeErr(w, 400, "bad_request")
		return
	}
	ss.touch()
	id, code := ss.addPeer(*m.Slot)
	switch code {
	case "":
		writeJSON(w, 201, map[string]any{"peerId": id, "slot": *m.Slot})
	case "bad_request":
		writeErr(w, 400, code)
	default: // not_multiplayer, room_full, slot_taken
		writeErr(w, 409, code)
	}
}

// removePeer: DELETE /v1/sessions/{id}/peers/{peerId} -> 200. Only that peer goes, whatever its slot (the session is ended by DELETE of the session).
func (s *server) removePeer(w http.ResponseWriter, r *http.Request) {
	ss := s.get(r.PathValue("id"))
	if ss == nil {
		writeErr(w, 404, "not_found")
		return
	}
	ss.touch()
	if !ss.removePeer(r.PathValue("peerId")) {
		writeErr(w, 404, "peer_not_found")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
