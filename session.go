// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func parseIPv4(s string) string {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return ip.To4().String()
}

type frame struct {
	buf []byte
	at  time.Time
}

type session struct {
	srv      *server
	id       string
	req      createReq
	core     string
	corePath string
	tc       *totvClient
	ci       coreInfo
	rwDir    string // PSP: the core's only writable directory (tmpfs): memory stick, shader cache
	iceIP    string
	dir      string
	created  time.Time

	mu             sync.Mutex
	state          string
	lastContact    time.Time
	everSignaled   bool
	connectedAt    time.Time
	playedBefore   time.Duration
	disconnectedAt time.Time

	// pmu guards the process fields set by start() (cmd, dir, rwDir) against stop()/teardown() running at the same time (DELETE during
	// a slow download); startEntered/startDone let stop() wait for a start() that is still running instead of for a child that never was.
	pmu          sync.Mutex
	startEntered bool
	startDone    chan struct{}
	exitedOnce   sync.Once
	cleanExit    atomic.Bool // the child said 'X' (bye): a deliberate end, as opposed to a crash or a kill

	// emulator child
	cmd       *exec.Cmd
	corePid   atomic.Int64 // the emulator itself (inside its PID namespace pid 1); cmd.Process is the supervisor in front of it
	wmu       sync.Mutex
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	fps       float64
	rate      float64
	flags     uint32
	helloCh   chan struct{}
	helloOnce sync.Once
	hostMsV   atomic.Pointer[hostMs]
	sramSize  atomic.Int64 // expected save RAM size, from the hello (0 = the core has none)
	upBusy    atomic.Bool  // at most one SRAM upload in flight
	netns     bool
	exited    chan struct{}
	errMsg    atomic.Value // string sent by the child ('E')

	upMu         sync.Mutex
	sramMu       sync.Mutex
	sram         []byte
	sramUploaded []byte

	rtc rtcState

	frames    chan frame
	pool      sync.Pool
	quit      chan struct{}
	stopOnce  sync.Once
	stopSaved bool
	stopErr   error
	tornDown  sync.Once

	// input
	imu       sync.Mutex
	haveSeq   bool
	lastSeq   uint16
	masks     [4]uint16
	axes      [4][4]int8
	lastVideo atomic.Int64 // unix nano of the last video frame from the child (stall watchdog)
	pendingIn atomic.Int64 // unix nano of the last input written to the child, until the next frame arrives

	// stats
	inRecv, inApplied, inBad, inOld atomic.Int64
	vFrames, vDropped, vEncoded     atomic.Int64
	aPackets                        atomic.Int64
	pdSent                          atomic.Int64
	bytesOut                        atomic.Int64
	inputToFrame, encodeMs, wireMs  *ring
	inputWrite                      *ring
}

func newSession(s *server, q createReq, core, corePath string, tc *totvClient, ip string) *session {
	ss := &session{srv: s, id: q.SessionID, req: q, core: core, corePath: corePath, tc: tc, iceIP: ip,
		created: time.Now(), state: "starting", lastContact: time.Now(),
		helloCh: make(chan struct{}), exited: make(chan struct{}), quit: make(chan struct{}), startDone: make(chan struct{}),
		frames:       make(chan frame, 2),
		inputToFrame: newRing(512), encodeMs: newRing(512), wireMs: newRing(512), inputWrite: newRing(512)}
	ss.errMsg.Store("")
	return ss
}

func (s *session) State() string { s.mu.Lock(); defer s.mu.Unlock(); return s.state }
func (s *session) setState(v string) {
	s.mu.Lock()
	if s.state != "stopping" && s.state != "stopped" {
		s.state = v
	}
	s.mu.Unlock()
}
func (s *session) touch() {
	s.mu.Lock()
	s.lastContact = time.Now()
	s.everSignaled = true
	s.mu.Unlock()
}

func (s *session) playedMs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.playedBefore
	if !s.connectedAt.IsZero() {
		d += time.Since(s.connectedAt)
	}
	return d.Milliseconds()
}

func (s *session) Status() map[string]any {
	return map[string]any{
		"state": s.State(), "playedMs": s.playedMs(),
		"stats": map[string]any{
			"inputRecv": s.inRecv.Load(), "inputApplied": s.inApplied.Load(), "inputBad": s.inBad.Load(), "inputOldDropped": s.inOld.Load(),
			"framesIn": s.vFrames.Load(), "framesDropped": s.vDropped.Load(), "framesEncoded": s.vEncoded.Load(), "audioPackets": s.aPackets.Load(),
			"playoutDelayExtID": s.rtc.extID.Load(), "playoutDelaySent": s.pdSent.Load(), "bytesOut": s.bytesOut.Load(),
			"msInputToFrame": s.inputToFrame.pct(), "msEncode": s.encodeMs.pct(), "msFrameToWire": s.wireMs.pct(), "msInputWrite": s.inputWrite.pct(),
			"sandboxFlags": s.flags, "hostMs": s.hostMsV.Load(),
		},
	}
}

// start downloads the ROM and save with the session token (parent only), then launches the sandboxed child. A stop() while it runs (DELETE of a
// session that is still downloading) cancels the context: the download is abandoned, no child is started, and start returns quickly.
func (s *session) start(ctx context.Context) string {
	s.pmu.Lock()
	select {
	case <-s.quit:
		s.pmu.Unlock()
		return "cancelled"
	default:
	}
	s.startEntered = true
	s.pmu.Unlock()
	defer close(s.startDone)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	return s.doStart(ctx)
}

func (s *session) doStart(ctx context.Context) string {
	dir, err := os.MkdirTemp(s.srv.cfg.TmpRoot, "ses-")
	if err != nil {
		return "internal"
	}
	s.pmu.Lock()
	s.dir = dir
	s.pmu.Unlock()
	ci, _ := s.srv.coreAllowed(s.core)
	s.ci = ci
	rom := filepath.Join(dir, "game.sfc")
	max := int64(64 << 20)
	if ci.fullPath {
		// the ROM lives alone in <dir>/rom: that directory is the ONLY thing the core may read (Landlock), the save directory is another one
		romDir := filepath.Join(dir, "rom")
		if err := os.Mkdir(romDir, 0o700); err != nil {
			return "internal"
		}
		rom = filepath.Join(romDir, "game.bin")
		max = ci.maxROM
	}
	if _, err := s.tc.download(ctx, "/api/games/rom/"+strconv.FormatInt(s.req.MediaID, 10), rom, max); err != nil {
		if errors.Is(err, errNotFound) {
			return "rom_not_found"
		}
		if errors.Is(err, errNoSpace) {
			log.Printf("session %s: not enough free space in %s for the ROM", s.id, s.srv.cfg.TmpRoot)
			return "no_space"
		}
		return "rom_download_failed"
	}
	if ci.fullPath {
		var err error
		if rom, err = finalizeFullPathROM(rom, s.req.Platform); err != nil {
			log.Printf("session %s: rom refused: %v", s.id, err)
			return "rom_invalid"
		}
	} else if err := normalizeROMFor(rom, s.req.Platform); err != nil {
		log.Printf("session %s: rom unpack failed: %v", s.id, err)
		return "rom_invalid"
	}
	srampath := "-"
	sp := filepath.Join(dir, "save.srm")
	if ci.dirSave {
		if err := s.srv.cfg.checkRWTmp(); err != nil {
			log.Printf("session %s: writable directory refused: %v", s.id, err)
			return "rw_tmp_unusable"
		}
		rw, err := os.MkdirTemp(s.srv.cfg.RWTmp, "rw-")
		if err != nil {
			return "internal"
		}
		s.pmu.Lock()
		s.rwDir = rw
		s.pmu.Unlock()
		if code := s.loadDirSave(ctx, sp); code != "" {
			return code
		}
	} else {
		switch _, err := s.tc.download(ctx, "/api/games/"+strconv.FormatInt(s.req.ItemID, 10)+"/sram", sp, 2<<20); {
		case err == nil:
			srampath = sp
			s.sramUploaded, _ = os.ReadFile(sp)
		case errors.Is(err, errNotFound):
		default:
			return "save_load_failed" // never start from an empty save when the real one could not be read
		}
	}
	sysdir := filepath.Join(dir, "system")
	_ = os.Mkdir(sysdir, 0o700)
	if ci.sysAssets {
		sysdir = s.srv.cfg.PSPSystemDir // read-only for the core (ro=), the PPSSPP assets
	}
	w, h := s.srv.size(ci)

	args := []string{s.corePath, rom, srampath, sysdir, strconv.Itoa(w), strconv.Itoa(h)}
	if o := s.srv.hostOptions(s.core, ci, s.rwDir, s.srv.cfg.PSPSystemDir); o != "" {
		args = append(args, o)
	}
	cmd := exec.Command(s.srv.cfg.EmuBin, args...)
	cmd.Env = []string{} // the child gets no environment and, above all, no token
	cmd.Dir = dir
	if lp := os.Getenv("TOTV_STREAM_CHILD_LOG"); lp != "" { // debugging only: the core's own log (PPSSPP, libretro log callback) to this file
		if lf, err := os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			cmd.Env, cmd.Stderr = []string{"RETRO_EMU_LOG=1"}, lf
			defer lf.Close()
		}
	}
	// a cgroup of its own for the session (cgroup.go): pids.max, memory.max, no swap
	cgWrapped := false
	if s.srv.cfg.cgroupMode() != "off" {
		if s.srv.cg.systemdRun != "" {
			s.srv.cfg.wrapCgroup(cmd, s.srv.cg.systemdRun, s.id)
			cgWrapped = true
		} else if s.srv.cfg.cgroupMode() == "require" {
			return "isolation_unavailable"
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "internal"
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "internal"
	}
	s.pmu.Lock()
	select { // a stop() that arrived while downloading: no child is started for a session that is already over
	case <-s.quit:
		s.pmu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		return "cancelled"
	default:
	}
	if err := startChild(cmd); err != nil {
		s.pmu.Unlock()
		return "emulator_start_failed"
	}
	s.cmd = cmd
	s.wmu.Lock()
	s.stdin = stdin
	s.wmu.Unlock()
	s.stdout = stdout
	go s.readLoop()
	s.pmu.Unlock()
	select {
	case <-s.helloCh:
	case <-s.exited:
		return "emulator_failed"
	case <-ctx.Done():
		return "emulator_timeout"
	}
	// do not trust what the child says about itself: the kernel's view of the process must show the filter
	// (Seccomp: 2) and no_new_privs. Landlock has no /proc field: it is covered by the child's flags plus --selftest.
	// the emulator is the child of the process we started when a PID namespace is in use (isolation.go: corePID)
	started := cmd.Process.Pid
	core := corePID(started)
	s.corePid.Store(int64(core))
	checks := []int{core}
	if core != started {
		checks = append(checks, started) // the supervisor is under the same filter
	}
	for _, p := range checks {
		if err := verifyChildSandbox(p); err != nil {
			log.Printf("session %s: sandbox check failed: %v", s.id, err)
			if !s.srv.cfg.AllowNoIsolation {
				_ = cmd.Process.Kill()
				return "isolation_unavailable"
			}
		}
	}
	if s.srv.cfg.PIDNSMode == "require" && (!s.srv.iso.PIDNS || s.flags&flagPIDNS == 0 || !childInOwnPIDNS(started)) { // refused even with AllowNoIsolation
		log.Printf("session %s: TOTV_STREAM_PIDNS=require and the emulator is not in its own PID namespace", s.id)
		_ = cmd.Process.Kill()
		return "isolation_unavailable"
	}
	if s.srv.iso.PIDNS && s.flags&flagPIDNS != 0 && !childInOwnPIDNS(started) { // the child says it is in its own PID namespace: /proc says otherwise
		log.Printf("session %s: PID namespace claimed by the child but not visible in /proc", s.id)
		if !s.srv.cfg.AllowNoIsolation {
			_ = cmd.Process.Kill()
			return "isolation_unavailable"
		}
	} else if s.srv.iso.PIDNS && s.flags&flagPIDNS == 0 && !s.srv.cfg.AllowNoIsolation {
		log.Printf("session %s: the host did not enter a PID namespace although the kernel allows it", s.id)
		_ = cmd.Process.Kill()
		return "isolation_unavailable"
	}
	if ci.gl || s.flags&flagGPU != 0 { // GPU core (the parent's own view, not just the child's flags): only /dev/nvidiactl may stay open (README, "GPU isolation (opção 2)")
		bad := gpuFDViolations(core)
		if strings.Contains(s.srv.gpu.Version, "NVIDIA") && s.flags&flagGPUFDS == 0 { // the NVIDIA driver is in use: the host must have reserved the closed descriptors
			bad = append(bad, "<the host did not report closing the GPU descriptors (flag 256)>")
		}
		if len(bad) > 0 {
			log.Printf("session %s: the emulator holds GPU descriptors it must not hold: %v", s.id, bad)
			if !s.srv.cfg.AllowNoIsolation {
				_ = cmd.Process.Kill()
				return "isolation_unavailable"
			}
		}
	}
	if cgWrapped { // the kernel's view of the session's cgroup, not systemd-run's word
		if err := s.srv.cfg.checkSessionCgroup("/sys/fs/cgroup", core, ownCgroupPath()); err != nil {
			s.srv.cgBad.Add(1)
			log.Printf("session %s: WARNING cgroup limits not enforced: %v", s.id, err)
			// require, and also auto once the probe proved the scope works (limits.session.state == "enforced"): auto only degrades at the start probe
			if cgroupMismatchRefuses(s.srv.cfg.cgroupMode(), s.srv.cg.l.State) {
				_ = cmd.Process.Kill()
				return "isolation_unavailable"
			}
		} else {
			s.srv.cgOK.Add(1)
		}
	}
	s.netns = childHasOwnNetns(core)
	const wantFlags = 3 // seccomp | landlock
	if s.flags&wantFlags != wantFlags && !s.srv.cfg.AllowNoIsolation {
		return "isolation_unavailable"
	}
	if s.flags&wantFlags != wantFlags {
		log.Printf("session %s: WARNING emulator running WITHOUT full isolation (dev mode, flags=%d)", s.id, s.flags)
	}
	go s.videoLoop()
	if ci.dirSave {
		go s.dirSaveLoop()
	}
	s.setState("waiting_signal")
	log.Printf("session %s: started core=%s sandboxFlags=%d", s.id, s.core, s.flags)
	return ""
}

func (s *session) getBuf(n int) []byte {
	if b, ok := s.pool.Get().([]byte); ok && cap(b) >= n {
		return b[:n]
	}
	return make([]byte, n)
}

// maxChildMsg is the largest payload accepted for a message type; -1 for types not allowed in that state.
// Before the hello only 'H' and 'E' are allowed. 'S' (save RAM) may never exceed the size announced in the hello.
func maxChildMsg(kind byte, vsize int, sramSize int64, gotHello bool) int64 {
	switch kind {
	case 'H':
		return 64
	case 'E':
		return 4096
	}
	if !gotHello {
		return -1
	}
	switch kind {
	case 'V':
		return int64(vsize)
	case 'A':
		return 1 << 20
	case 'S':
		if sramSize <= 0 {
			return -1
		}
		return sramSize
	case 'T':
		return 48
	case 'X':
		return 0
	}
	return -1
}

func (s *session) readLoop() {
	defer s.closeExited()
	br := bufio.NewReaderSize(s.stdout, 1<<16)
	var hdr [8]byte
	rs := newResampler()
	ow, oh := s.srv.size(s.ci)
	var vsize = ow * oh * 3 / 2
	gotHello := false
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		n64 := int64(binary.LittleEndian.Uint32(hdr[4:]))
		if n64 > maxChildMsg(hdr[0], vsize, s.sramSize.Load(), gotHello) {
			log.Printf("session %s: child message %q of %d bytes violates the protocol, closing it", s.id, hdr[0], n64)
			return
		}
		if hdr[0] == 'X' { // bye: nothing else is read from the child, whatever it writes while exiting
			s.cleanExit.Store(true)
			return
		}
		n := int(n64)
		if hdr[0] == 'V' {
			if n != vsize {
				return
			}
			buf := s.getBuf(n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			now := time.Now()
			if t := s.pendingIn.Swap(0); t != 0 {
				s.inputToFrame.add(now.Sub(time.Unix(0, t)))
			}
			s.vFrames.Add(1)
			s.lastVideo.Store(now.UnixNano())
			f := frame{buf, now}
			for sent := false; !sent; {
				select {
				case s.frames <- f:
					sent = true
				default:
					select { // drop the OLDEST frame: freshness over completeness
					case old := <-s.frames:
						s.vDropped.Add(1)
						s.pool.Put(old.buf)
					default:
					}
				}
			}
			continue
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(br, p); err != nil {
			return
		}
		switch hdr[0] {
		case 'H':
			if !gotHello && len(p) >= 32 { // a second hello is ignored: it must not rewrite what the checks saw
				gotHello = true
				s.sramSize.Store(int64(binary.LittleEndian.Uint32(p[24:])))
				s.fps = math.Float64frombits(binary.LittleEndian.Uint64(p[0:]))
				s.rate = math.Float64frombits(binary.LittleEndian.Uint64(p[8:]))
				s.flags = binary.LittleEndian.Uint32(p[28:])
				rs.setRate(s.rate)
				s.lastVideo.Store(time.Now().UnixNano())
				s.helloOnce.Do(func() { close(s.helloCh) })
			}
		case 'A':
			s.onAudio(rs, p)
		case 'S':
			s.sramMu.Lock()
			s.sram = p
			s.sramMu.Unlock()
			if s.upBusy.CompareAndSwap(false, true) { // one upload at a time; the latest data is kept for the next one
				go func() { defer s.upBusy.Store(false); _ = s.uploadSRAM(context.Background()) }()
			}
		case 'T': // per-stage host timing (12 float32), every 120 frames
			if len(p) == 48 {
				var h hostMs
				_ = binary.Read(bytes.NewReader(p), binary.LittleEndian, &h)
				s.hostMsV.Store(&h)
			}
		case 'E':
			s.errMsg.Store(string(p))
		}
	}
}

// uploadSRAM sends the latest save RAM if it differs from what the Totv already has. Returns true if it uploaded.
func (s *session) uploadSRAM(ctx context.Context) error {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	s.sramMu.Lock()
	data := s.sram
	s.sramMu.Unlock()
	if len(data) == 0 || bytes.Equal(data, s.sramUploaded) {
		return nil
	}
	if allZero(data) && s.sramUploaded == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.tc.putSRAM(ctx, s.req.ItemID, "server_"+s.core, s.req.MediaID, data); err != nil {
		return err
	}
	s.sramUploaded = data
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func (s *session) sendChild(m [4]byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.stdin != nil {
		_, _ = s.stdin.Write(m[:])
	}
}

func (s *session) closeExited() { s.exitedOnce.Do(func() { close(s.exited) }) }

// process returns the child once start() has launched it (nil before, and for a session that never got that far).
func (s *session) process() *os.Process {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if s.cmd == nil {
		return nil
	}
	return s.cmd.Process
}

// errSaveSkipped: the core did not end cleanly and its files were still changing: the last periodic upload stays as the save.
var errSaveSkipped = errors.New("save skipped after an unclean exit")

// stop saves (optionally) and ends the session. Safe to call from several goroutines. A session whose child was never started (DELETE while
// start() is still downloading) does not wait for a process: quit cancels start(), which returns at once.
func (s *session) stop(save bool) (bool, error) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.state = "stopping"
		s.mu.Unlock()
		close(s.quit)
		s.rtc.streaming.Store(false)
		s.pmu.Lock() // start() checks quit under this lock before launching: either it launched already (cmd is set) or it never will
		started, entered := s.cmd != nil, s.startEntered
		s.pmu.Unlock()
		if started {
			s.sendChild([4]byte{2})
			select {
			case <-s.exited:
			case <-time.After(10 * time.Second):
				if p := s.process(); p != nil {
					_ = p.Kill()
				}
				<-s.exited
			}
		}
		if entered {
			select { // start() returns promptly once quit is closed (its context is cancelled); only then are dir/rwDir final
			case <-s.startDone:
			case <-time.After(5 * time.Second):
				log.Printf("session %s: start() did not return after the stop", s.id)
			}
		}
		if save && started {
			skipUpload := false
			crashed := !s.cleanExit.Load()
			if s.ci.dirSave {
				// a clean exit: the files are final, no quiet period. After a crash or a kill the core may have died in the middle of a
				// write: only files that have been quiet for a while are packed, else the last periodic upload stays the save.
				quiet := time.Duration(0)
				if crashed {
					quiet = 2 * time.Second
				}
				if err := s.refreshDirSave(quiet); errors.Is(err, errSaveBusy) {
					log.Printf("session %s: core did not exit cleanly and its save files changed a moment ago: not packed", s.id)
					skipUpload = true
				} else if err != nil {
					log.Printf("session %s: save package: %v", s.id, err)
					s.stopErr = err
				}
			} else if crashed {
				// SRAM cores: what the host sent while it was alive was already uploaded as it arrived; nothing is added after a crash
				skipUpload = true
			}
			if !skipUpload {
				s.sramMu.Lock()
				had := len(s.sram) > 0
				s.sramMu.Unlock()
				if s.stopErr == nil {
					s.stopErr = s.uploadSRAM(context.Background())
				}
				s.stopSaved = had && s.stopErr == nil
			}
		}
		s.teardown()
		s.mu.Lock()
		s.state = "stopped"
		s.mu.Unlock()
		s.srv.drop(s.id)
		log.Printf("session %s: stopped saved=%v", s.id, s.stopSaved)
	})
	return s.stopSaved, s.stopErr
}

// teardown releases everything (also used when start fails).
func (s *session) teardown() {
	s.tornDown.Do(func() {
		s.pmu.Lock()
		cmd, dir, rw := s.cmd, s.dir, s.rwDir
		s.pmu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			go func(c *exec.Cmd) { _ = c.Wait() }(cmd)
		} else {
			s.closeExited() // never started: whoever waits for the process is released
		}
		s.closeRTC()
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		if rw != "" {
			_ = os.RemoveAll(rw) // memory stick and shader cache: gone with the session
		}
	})
}

// monitor ends sessions nobody signals (proposal: 60 s without any signal, 30 s after the peer is lost).
func (s *session) monitor() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-s.exited:
			go s.stop(true)
			return
		case <-t.C:
			s.mu.Lock()
			never := !s.everSignaled && time.Since(s.created) > s.srv.cfg.NoSignalTimeout
			lost := !s.disconnectedAt.IsZero() && time.Since(s.disconnectedAt) > s.srv.cfg.DisconnectGrace
			s.mu.Unlock()
			// the child is alive but the core stopped producing video (bad ROM, core hang): end it instead of
			// leaving a frozen "playing" session. Nothing is saved: the SRAM of a stalled core cannot be trusted.
			if lv := s.lastVideo.Load(); lv != 0 && time.Since(time.Unix(0, lv)) > s.srv.cfg.noVideo() {
				log.Printf("session %s: emulator produced no video for %s, ending it (check the ROM and core)", s.id, s.srv.cfg.noVideo())
				if p := s.process(); p != nil {
					_ = p.Kill() // a stuck core will not read the quit message
				}
				go s.stop(false)
				return
			}
			if never || lost {
				log.Printf("session %s: ended by timeout", s.id)
				go s.stop(true)
				return
			}
		}
	}
}

func (s *session) onInput(msg []byte) {
	s.inRecv.Add(1)
	t0 := time.Now()
	p, ok := parseInput(msg)
	if !ok {
		s.inBad.Add(1)
		return
	}
	s.imu.Lock()
	if s.haveSeq && !newerSeq(p.seq, s.lastSeq) {
		s.imu.Unlock()
		s.inOld.Add(1)
		return
	}
	s.haveSeq, s.lastSeq = true, p.seq
	m := retroMaskFor(s.req.Platform, p.buttons)
	changed := m != s.masks[p.port]
	s.masks[p.port] = m
	var axisMsgs [][4]byte
	if hasAnalog(s.req.Platform) { // analog sticks: one message per axis that changed (int8, Y positive = down)
		for i := 0; i < 4; i++ {
			if p.axes[i] != s.axes[p.port][i] {
				s.axes[p.port][i] = p.axes[i]
				axisMsgs = append(axisMsgs, [4]byte{3, p.port, byte(i), byte(p.axes[i])})
			}
		}
	}
	s.imu.Unlock()
	s.inApplied.Add(1)
	for _, am := range axisMsgs {
		s.pendingIn.CompareAndSwap(0, time.Now().UnixNano())
		s.sendChild(am)
	}
	if changed {
		s.pendingIn.CompareAndSwap(0, time.Now().UnixNano())
		s.sendChild([4]byte{1, byte(m), byte(m >> 8), p.port})
		s.inputWrite.add(time.Since(t0))
	}
}

func (s *session) releaseInput() {
	s.imu.Lock()
	for i := range s.masks {
		if s.masks[i] != 0 {
			s.masks[i] = 0
			s.sendChild([4]byte{1, 0, 0, byte(i)})
		}
		for a := range s.axes[i] {
			if s.axes[i][a] != 0 {
				s.axes[i][a] = 0
				s.sendChild([4]byte{3, byte(i), byte(a), 0})
			}
		}
	}
	s.imu.Unlock()
}

var _ = json.Marshal

// finalizeFullPathROM names the downloaded file by what it is (PPSSPP picks the loader by extension): ISO 9660 image, CSO or PBP.
// A zip is refused (an ISO inside a zip would have to be unpacked to GB of disk; the Totv serves the file itself).
func finalizeFullPathROM(p, platform string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	f.Close()
	ext, err := pspROMExt(head[:n])
	if err != nil {
		return "", err
	}
	np := filepath.Join(filepath.Dir(p), "game"+ext)
	if err := os.Rename(p, np); err != nil {
		return "", err
	}
	return np, nil
}

// pspROMExt looks at the first bytes: "CISO" = CSO, "\0PBP" = PBP, a zip is refused, anything else is treated as an ISO (PPSSPP
// itself validates the ISO 9660 volume descriptor when it opens it).
func pspROMExt(head []byte) (string, error) {
	switch {
	case len(head) < 4:
		return "", errors.New("rom too small")
	case string(head[:4]) == "CISO":
		return ".cso", nil
	case string(head[:4]) == "\x00PBP":
		return ".pbp", nil
	case isZip(head):
		return "", errors.New("zip not supported for this platform")
	}
	return ".iso", nil
}

// loadDirSave downloads the save package (a zip) and restores it below the core's writable directory. 404 = no save yet.
// Any other failure refuses the session: never start from an empty save when the real one could not be read or understood.
func (s *session) loadDirSave(ctx context.Context, tmp string) string {
	switch n, err := s.tc.download(ctx, "/api/games/"+strconv.FormatInt(s.req.ItemID, 10)+"/sram", tmp, pspSaveLimits.MaxPacked); {
	case errors.Is(err, errNotFound):
		return ""
	case err != nil:
		return "save_load_failed"
	default:
		z, err := os.ReadFile(tmp)
		_ = os.Remove(tmp)
		if err != nil || int64(len(z)) != n {
			return "save_load_failed"
		}
		if err := unzipSave(z, s.rwDir, pspSavePrefix, pspSaveLimits); err != nil {
			log.Printf("session %s: save package refused: %v", s.id, err)
			return "save_load_failed"
		}
		s.sramUploaded = z
	}
	return ""
}

// refreshDirSave packs the memory stick's savedata into s.sram (the buffer uploadSRAM compares and sends). With quiet > 0 the
// files must have been untouched for that long (the core may be writing); errSaveBusy then means "try again later".
func (s *session) refreshDirSave(quiet time.Duration) error {
	z, err := zipSaveDir(s.rwDir, pspSavePrefix, pspSaveLimits, quiet)
	if err != nil {
		return err
	}
	if z == nil {
		return nil // no savedata yet: nothing to upload, and an old save is never replaced by nothing
	}
	s.sramMu.Lock()
	s.sram = z
	s.sramMu.Unlock()
	return nil
}

// dirSaveLoop is the periodic upload for directory saves (the SRAM cores get theirs from the host's 'S' message).
func (s *session) dirSaveLoop() {
	t := time.NewTicker(s.srv.dirSaveEvery())
	defer t.Stop()
	qt := time.NewTicker(s.srv.quotaEvery())
	defer qt.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-s.exited:
			return
		case <-qt.C:
			// the writable directory is a tmpfs (RAM): a core that fills it is ended, and nothing it wrote is uploaded
			if used, files := dirUsage(s.rwDir); used > s.srv.cfg.rwMax() || files > s.srv.cfg.rwFilesMax() {
				log.Printf("session %s: the core's writable directory holds %d bytes in %d entries, over the limit (%d bytes, %d entries): ending the session", s.id, used, files, s.srv.cfg.rwMax(), s.srv.cfg.rwFilesMax())
				if p := s.process(); p != nil {
					_ = p.Kill()
				}
				go s.stop(false)
				return
			}
		case <-t.C:
			if err := s.refreshDirSave(2 * time.Second); err != nil {
				if !errors.Is(err, errSaveBusy) {
					log.Printf("session %s: periodic save: %v", s.id, err)
				}
				continue
			}
			if s.upBusy.CompareAndSwap(false, true) {
				go func() { defer s.upBusy.Store(false); _ = s.uploadSRAM(context.Background()) }()
			}
		}
	}
}

func (s *server) quotaEvery() time.Duration {
	if s.cfg.QuotaEvery > 0 {
		return s.cfg.QuotaEvery
	}
	return 5 * time.Second
}

func (s *server) dirSaveEvery() time.Duration {
	if s.cfg.DirSaveEvery > 0 {
		return s.cfg.DirSaveEvery
	}
	return 30 * time.Second
}

// ---- child start with PDEATHSIG (hardening, revision 3) ----
// PR_SET_PDEATHSIG fires when the THREAD that created the child exits, not when the process does. A goroutine runs on whatever OS thread the scheduler
// picks, and Go may retire a thread at any time, so a plain Pdeathsig would kill a healthy session now and then. All children are therefore started
// from ONE goroutine that locks its OS thread for the life of the component: that thread only dies when the whole component does (crash, SIGKILL,
// panic), and then the kernel kills every emulator child, even one stuck inside a core and not reading its stdin (the pipe EOF alone would not reach it).
var spawnReq = make(chan spawnJob)
var spawnOnce sync.Once

type spawnJob struct {
	cmd  *exec.Cmd
	done chan error
}

func spawnLoop() {
	runtime.LockOSThread() // never unlocked: the thread belongs to this loop until the process ends
	for j := range spawnReq {
		j.done <- j.cmd.Start()
	}
}

// startChild is cmd.Start() with Pdeathsig=SIGKILL, from the dedicated locked thread.
func startChild(cmd *exec.Cmd) error {
	spawnOnce.Do(func() { go spawnLoop() })
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	j := spawnJob{cmd: cmd, done: make(chan error, 1)}
	spawnReq <- j
	return <-j.done
}

// flags of the child's hello (emu/host.c)
const (
	flagGPU    = 16
	flagGPUFDS = 256
	flagPIDNS  = 512
)

// corePID is the pid (as seen from here) of the emulator: the one to inspect and to stop/continue. Before the hello it is the process we started.
func (s *session) corePID() int {
	if p := int(s.corePid.Load()); p > 0 {
		return p
	}
	return s.cmd.Process.Pid
}
