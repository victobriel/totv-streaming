package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
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
	mux.HandleFunc("POST /v1/sessions/{id}/signal", s.signal)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.remove)
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
		if ci, ok := s.coreAllowed(name); ok && (!ci.gl || s.gpu.OK) && (!ci.sysAssets || s.assetsOK()) {
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
	writeJSON(w, 200, map[string]any{"version": version, "encoder": "x264", "gpu": gpu, "cores": s.availableCores(), "isolation": s.iso, "limits": s.limitsView()})
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
	SessionID string `json:"sessionId"`
	ItemID    int64  `json:"itemId"`
	MediaID   int64  `json:"mediaId"`
	Platform  string `json:"platform"`
	Core      string `json:"core"`
	Token     string `json:"token"`
	ServerURL string `json:"serverUrl"`
	IceHost   string `json:"iceHost"`
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
	if !idRe.MatchString(q.SessionID) || q.ItemID <= 0 || q.MediaID <= 0 || (coreOK && q.Platform != ci.platform) || !coreRe.MatchString(core) {
		writeErr(w, 400, "bad_request")
		return
	}
	if !coreOK || (ci.gl && !s.gpu.OK) {
		writeErr(w, 400, "unknown_core")
		return
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
	if err != nil || ip == "" {
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
		ans, code := ss.handleOffer(r.Context(), m.SDP)
		if code != "" {
			writeErr(w, 400, code)
			return
		}
		writeJSON(w, 200, map[string]string{"type": "answer", "sdp": ans})
	case len(m.Candidate) > 0 && string(m.Candidate) != "null":
		if code := ss.handleCandidate(m.Candidate); code != "" {
			writeErr(w, 400, code)
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

// limitsView is `limits` of /v1/health: the component's own cgroup (as before, flattened) plus `session`, the per-session cgroup.
func (s *server) limitsView() any {
	ss := s.cg.l
	ss.Verified, ss.Violation = int(s.cgOK.Load()), int(s.cgBad.Load())
	return struct {
		cgroupLimits
		Session sessionLimits `json:"session"`
	}{s.limits, ss}
}
