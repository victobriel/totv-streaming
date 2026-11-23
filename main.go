// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

// totv-streaming: streaming component for Totv (SNES prototype). GPL-3.0-or-later.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const version = "0.1.0-proto1"

type config struct {
	SockPath, SecretFile, CoresDir, EmuBin, TmpRoot  string
	UDPPort                                          int
	DevTestCore, AllowNoIsolation                    bool
	GPUDevices                                       string // device nodes a HW_RENDER core may still open after the sandbox
	ReadbackPBO, N64Internal2x                       bool
	N64CoreOpts                                      string // extra mupen64plus core options "k=v;k=v" (experiments)
	PS2BiosDir, BigTmp, PS2CoreOpts                  string // PS2: operator's BIOS directory (read by the parent, copied per session), disk directory for big images, extra core options
	PS2MaxISOMB                                      int    // PS2 image ceiling in MiB (0 = 9 GiB)
	PSPSystemDir, RWTmp, PSPCoreOpts                 string // PSP: read-only dir that holds PPSSPP/ (assets), writable tmpfs root for the memory stick, extra core options
	N64Overscan, N64Aspect                           string // "l,r,t,b" crop in N64 320x240 pixels ("" = default, "off"); "" or "16:9" widescreen
	MaxSessions, W, H, Kbps                          int
	NoSignalTimeout, DisconnectGrace, NoVideoTimeout time.Duration
	DirSaveEvery                                     time.Duration // periodic upload of directory saves (0 = 30 s)
	RWFiles                                          int           // most entries (inodes) in a session's writable directory (0 = 20000)
	RWMax, FileMax                                   int64         // quota of a session's writable directory; RLIMIT_FSIZE of the child (bytes; 0 = 512 MiB)
	QuotaEvery                                       time.Duration // how often the quota is measured (0 = 5 s)
	RWAllowDisk                                      bool          // development: TOTV_STREAM_RW_TMP may be a plain disk directory
	CgroupMode                                       string        // TOTV_STREAM_CGROUP: auto | off | require ("" = off): a cgroup of its own for each session (cgroup.go)
	PIDNSMode                                        string        // TOTV_STREAM_PIDNS: auto (default) | require (a session is refused without a PID namespace)
	SessionPids, SessionMemMB                        int           // per-session pids.max and memory.max (0 = defaults)
}

func (c config) noVideo() time.Duration {
	if c.NoVideoTimeout > 0 {
		return c.NoVideoTimeout
	}
	return 5 * time.Second
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func envInt(k string, d int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return d
}

func loadConfig() config {
	exe, _ := os.Executable()
	return config{
		SockPath:         env("TOTV_STREAM_SOCK", "/run/totv-stream/control.sock"),
		SecretFile:       env("TOTV_STREAM_SECRET_FILE", "/run/totv-stream-secret/retro-stream.secret"),
		CoresDir:         env("TOTV_STREAM_CORES", "/opt/cores"),
		EmuBin:           env("TOTV_STREAM_EMU", filepath.Join(filepath.Dir(exe), "retro-emu")),
		TmpRoot:          env("TOTV_STREAM_TMP", os.TempDir()),
		UDPPort:          envInt("TOTV_STREAM_UDP_PORT", 8422),
		DevTestCore:      os.Getenv("TOTV_STREAM_DEV_TESTCORE") == "1",
		AllowNoIsolation: os.Getenv("TOTV_STREAM_ALLOW_NO_ISOLATION") == "1",
		GPUDevices:       env("TOTV_STREAM_GPU_DEVICES", "/dev/nvidiactl"),
		ReadbackPBO:      os.Getenv("TOTV_STREAM_READBACK") == "pbo",
		N64Internal2x:    os.Getenv("TOTV_STREAM_N64_RES") == "2x",
		N64CoreOpts:      os.Getenv("TOTV_STREAM_N64_CORE_OPTS"),
		N64Overscan:      os.Getenv("TOTV_STREAM_N64_OVERSCAN"),
		PSPSystemDir:     env("TOTV_STREAM_PSP_SYSTEM", filepath.Join(env("TOTV_STREAM_CORES", "/opt/cores"), "system")),
		RWTmp:            env("TOTV_STREAM_RW_TMP", env("TOTV_STREAM_TMP", os.TempDir())),
		PSPCoreOpts:      os.Getenv("TOTV_STREAM_PSP_CORE_OPTS"),
		PS2BiosDir:       os.Getenv("TOTV_STREAM_PS2_BIOS_DIR"),
		BigTmp:           os.Getenv("TOTV_STREAM_BIG_TMP"),
		PS2CoreOpts:      os.Getenv("TOTV_STREAM_PS2_CORE_OPTS"),
		PS2MaxISOMB:      envInt("TOTV_STREAM_PS2_MAX_ISO_MB", 0),
		N64Aspect:        os.Getenv("TOTV_STREAM_N64_ASPECT"),
		MaxSessions:      envInt("TOTV_STREAM_MAX_SESSIONS", 1),
		W:                envInt("TOTV_STREAM_WIDTH", 640),
		H:                envInt("TOTV_STREAM_HEIGHT", 480),
		Kbps:             envInt("TOTV_STREAM_KBPS", 4000),
		NoSignalTimeout:  time.Duration(envInt("TOTV_STREAM_NOSIGNAL_SEC", 60)) * time.Second,
		DisconnectGrace:  time.Duration(envInt("TOTV_STREAM_DISCONNECT_SEC", 30)) * time.Second,
		NoVideoTimeout:   time.Duration(envInt("TOTV_STREAM_NOVIDEO_SEC", 5)) * time.Second,
		RWFiles:          envInt("TOTV_STREAM_RW_MAX_FILES", defaultRWFiles),
		RWMax:            int64(envInt("TOTV_STREAM_RW_MAX_MB", defaultRWMax>>20)) << 20,
		FileMax:          int64(envInt("TOTV_STREAM_MAX_FILE_MB", defaultFileMax>>20)) << 20,
		RWAllowDisk:      os.Getenv("TOTV_STREAM_RW_TMP_ALLOW_DISK") == "1",
		CgroupMode:       env("TOTV_STREAM_CGROUP", "auto"),
		PIDNSMode:        env("TOTV_STREAM_PIDNS", "auto"),
		SessionPids:      envInt("TOTV_STREAM_SESSION_PIDS", defaultSessionPids),
		SessionMemMB:     envInt("TOTV_STREAM_SESSION_MEM_MB", defaultSessionMemMB),
	}
}

type isolation struct {
	Seccomp  bool `json:"seccomp"`
	Landlock bool `json:"landlock"`
	Netns    bool `json:"netns"`
	// revision 3: what exactly protects the parent from a signal sent through the child's descriptors, and the other new denials
	LandlockABI       int  `json:"landlock_abi"`
	LandlockScope     bool `json:"landlock_scope"`      // Landlock signal scope (ABI >= 6, kernel 6.12+)
	AsyncOwnerBlocked bool `json:"async_owner_blocked"` // seccomp refuses F_SETOWN/F_SETSIG/F_SETOWN_EX/FIOSETOWN/SIOCSPGRP: the equivalent protection on older kernels
	IPCNS             bool `json:"ipcns"`               // own SysV IPC namespace (otherwise SysV IPC is denied outright)
	ForkBlocked       bool `json:"fork_blocked"`        // no new processes, threads only
	PIDNS             bool `json:"pidns"`               // the emulator is pid 1 of its own PID namespace (GPU isolation, option 2)
}

// probeIsolation runs `retro-emu --selftest`: the child applies the sandbox to itself and tries a forbidden
// socket()/ptrace()/open(). A flag is true ONLY when the attempt was really blocked.
func probeIsolation(emu string) isolation {
	out, err := exec.Command(emu, "--selftest").Output()
	var r struct {
		Seccomp           bool `json:"seccomp"`
		Landlock          bool `json:"landlock"`
		Netns             bool `json:"netns"`
		LandlockABI       int  `json:"landlock_abi"`
		LandlockScope     bool `json:"landlock_scope"`
		AsyncOwnerBlocked bool `json:"async_owner_blocked"`
		IPCNS             bool `json:"ipcns"`
		ForkBlocked       bool `json:"fork_blocked"`
		PIDNS             bool `json:"pidns"`
	}
	if err != nil || json.Unmarshal(out, &r) != nil {
		return isolation{}
	}
	return isolation{Seccomp: r.Seccomp, Landlock: r.Landlock, Netns: r.Netns, LandlockABI: r.LandlockABI, LandlockScope: r.LandlockScope,
		AsyncOwnerBlocked: r.AsyncOwnerBlocked, IPCNS: r.IPCNS, ForkBlocked: r.ForkBlocked, PIDNS: r.PIDNS}
}

// gpuInfo is what `retro-emu --gltest` reports: whether an EGL/GL context can be created on this machine.
type gpuInfo struct {
	OK       bool   `json:"ok"`
	Renderer string `json:"renderer,omitempty"`
	Version  string `json:"version,omitempty"`
	Error    string `json:"error,omitempty"`
}

func probeGPU(emu string) gpuInfo {
	out, err := exec.Command(emu, "--gltest").Output()
	var r gpuInfo
	if err != nil || json.Unmarshal(out, &r) != nil {
		return gpuInfo{}
	}
	return r
}

func main() {
	cfg := loadConfig()
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) // parent holds the token: no ptrace/core dump
	srv := newServer(cfg, probeIsolation(cfg.EmuBin))
	srv.gpu = probeGPU(cfg.EmuBin)
	log.Printf("totv-streaming %s isolation=%+v", version, srv.iso)
	if !(srv.iso.Seccomp && srv.iso.Landlock) && !cfg.AllowNoIsolation {
		log.Printf("isolation not active: sessions are refused (set TOTV_STREAM_ALLOW_NO_ISOLATION=1 for development only)")
	}
	for _, w := range srv.limits.Warnings {
		log.Printf("WARNING: %s", w)
	}
	logCgroup(srv.cg)
	if cfg.PIDNSMode == "require" && !srv.iso.PIDNS {
		log.Printf("WARNING: TOTV_STREAM_PIDNS=require but the host cannot enter a PID namespace here: every session will be refused")
	} else if cfg.PIDNSMode != "require" && !srv.iso.PIDNS {
		log.Printf("WARNING: no PID namespace (sessions run without it; TOTV_STREAM_PIDNS=require would refuse them)")
	}
	if srv.iso.Seccomp && !srv.iso.LandlockScope && !srv.iso.AsyncOwnerBlocked {
		log.Printf("WARNING: nothing protects the component from a signal sent through the child's descriptors (Landlock ABI %d, seccomp async-owner denial not proved)", srv.iso.LandlockABI)
	}
	if srv.iso.Netns && !srv.iso.IPCNS {
		log.Printf("WARNING: no own IPC namespace (the container's seccomp profile predates CLONE_NEWIPC: regenerate docker/seccomp-totv.json); SysV IPC is denied instead")
	}
	if err := cfg.checkRWTmp(); err != nil {
		log.Printf("WARNING: PSP sessions will be refused: %v", err)
	}
	if srv.ps2Present() {
		if r := ps2BiosScan(cfg.PS2BiosDir); !r.OK {
			log.Printf("WARNING: PS2 sessions will be refused (%s): TOTV_STREAM_PS2_BIOS_DIR=%q holds no valid PS2 BIOS (the component never ships one)", r.Reason, cfg.PS2BiosDir)
		}
		if w := cfg.checkBigTmp(); w != "" {
			log.Printf("WARNING: %s", w)
		}
	}
	for _, d := range []string{cfg.TmpRoot, cfg.RWTmp, cfg.bigTmp()} {
		if n := cleanOrphans(d); n > 0 {
			log.Printf("removed %d session directories left in %s by a previous run", n, d)
		}
	}
	ln, err := listenUnix(cfg.SockPath)
	if err != nil {
		log.Fatalf("socket: %v", err)
	}
	hs := &http.Server{Handler: srv.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		srv.stopAll(ctx)
		_ = hs.Shutdown(ctx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func listenUnix(path string) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("path exists and is not a socket")
		}
		_ = os.Remove(path)
	}
	old := syscall.Umask(0o117) // socket born 0660
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o660)
	return ln, nil
}
