// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

// Per-session cgroup (GPU isolation, option 2). Each emulator child runs in its OWN transient systemd scope of the user manager
// (`systemd-run --user --scope -p TasksMax= -p MemoryMax= -p MemorySwapMax=0`): the scope is a cgroup below user@<uid>.service, which systemd delegates
// to the user (cgroup v2: cpu, memory, pids), so no root is involved and nothing outside the user's own subtree is touched. `systemd-run --scope` does not
// fork: it moves itself into the new scope and exec()s the command, so the process the component started is the emulator (or its PID-namespace supervisor)
// with the same pid; `env -i` then clears the environment that systemd-run needed (XDG_RUNTIME_DIR, bus address), because the emulator must get none.
//
// What it bounds: threads (pids.max; the seccomp filter already forbids processes), RAM (memory.max, which also counts the private /dev/shm tmpfs of PSP) and
// swap (memory.swap.max=0), per session. A core that goes over is killed by the kernel inside ITS scope; the component and the other sessions are not.
//
// Modes (TOTV_STREAM_CGROUP): auto (default: use it when the probe at start proves it works, otherwise say so in /v1/health and the log), off, require (a
// session is refused when it cannot be put in an enforced cgroup). Without a user manager (the component run as a system service: see contrib/) the unit's own
// MemoryMax=/TasksMax= bound the whole component instead, which cgroupLimitsAt reports as before.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSessionPids  = 256  // TOTV_STREAM_SESSION_PIDS: threads of one session (PPSSPP + NVIDIA driver use a few dozen)
	defaultSessionMemMB = 3072 // TOTV_STREAM_SESSION_MEM_MB: RSS measured 220-350 MB (N64/PSP); the PSP private tmpfs and the driver's pinned memory count too
)

// sessionLimits is the `limits.session` part of /v1/health.
type sessionLimits struct {
	Mode      string   `json:"mode"`               // auto | off | require
	State     string   `json:"state"`              // "enforced" | "not enforced"
	Method    string   `json:"method,omitempty"`   // "systemd-run --user --scope"
	PidsMax   int      `json:"pids_max,omitempty"` // per session
	MemoryMB  int      `json:"memory_max_mb,omitempty"`
	SwapZero  bool     `json:"swap_max_zero,omitempty"`
	Verified  int      `json:"sessions_verified"`   // sessions whose cgroup limits were read back from the kernel and matched
	Violation int      `json:"sessions_unverified"` // sessions that were started and could NOT be proved to be inside the limits
	Warnings  []string `json:"warnings"`
}

// cgSettings is what the server decided at start; sessions only read it.
type cgSettings struct {
	l          sessionLimits
	systemdRun string // path; "" when not usable
	uid        int
}

func (c config) sessionPids() int {
	if c.SessionPids > 0 {
		return c.SessionPids
	}
	return defaultSessionPids
}
func (c config) sessionMemMB() int {
	if c.SessionMemMB > 0 {
		return c.SessionMemMB
	}
	return defaultSessionMemMB
}

// cgroupProps are the properties given to the scope (also what the probe and the verification expect to read back).
func (c config) cgroupProps() []string {
	return []string{"TasksMax=" + strconv.Itoa(c.sessionPids()), "MemoryMax=" + strconv.Itoa(c.sessionMemMB()) + "M", "MemorySwapMax=0", "OOMPolicy=kill"}
}

// userBusEnv is what `systemd-run --user` needs to find the user manager. The emulator never sees it (env -i).
func userBusEnv() []string {
	e := []string{}
	rd := os.Getenv("XDG_RUNTIME_DIR")
	if rd == "" {
		rd = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	e = append(e, "XDG_RUNTIME_DIR="+rd)
	if v := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); v != "" {
		e = append(e, "DBUS_SESSION_BUS_ADDRESS="+v)
	}
	return e
}

// wrapCgroup rewrites cmd (retro-emu args...) into `systemd-run ... -- env -i [RETRO_EMU_LOG=1] retro-emu args...`.
func (c config) wrapCgroup(cmd *exec.Cmd, sysdRun, id string) {
	envBin, err := exec.LookPath("env")
	if err != nil {
		envBin = "/usr/bin/env"
	}
	inner := append([]string{}, cmd.Args...) // Args[0] = path of retro-emu
	inner[0] = cmd.Path
	keep := []string{}
	for _, e := range cmd.Env { // the only variable the component ever gives the child (debug log), carried through `env -i`
		keep = append(keep, e)
	}
	args := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "--description=totv-streaming session " + sanitizeUnit(id)}
	for _, p := range c.cgroupProps() {
		args = append(args, "-p", p)
	}
	args = append(args, "--", envBin, "-i", "--")
	args = append(args, keep...)
	args = append(args, inner...)
	cmd.Path, cmd.Args = sysdRun, args
	cmd.Env = userBusEnv()
}

func sanitizeUnit(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			b[i] = '_'
		}
	}
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}

// readCgroupOf returns the cgroup v2 path of pid ("/user.slice/...") and the three limit files read back from the kernel.
func readCgroupOf(root string, pid int) (path, pids, mem, swap string, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(ln, "0::"); ok {
			path = filepath.Clean("/" + p)
		}
	}
	if path == "" {
		err = fmt.Errorf("no cgroup v2 line in /proc/%d/cgroup", pid)
		return
	}
	rd := func(n string) string {
		x, e := os.ReadFile(filepath.Join(root, path, n))
		if e != nil {
			return ""
		}
		return strings.TrimSpace(string(x))
	}
	return path, rd("pids.max"), rd("memory.max"), rd("memory.swap.max"), nil
}

// checkSessionCgroup compares the kernel's view of pid's cgroup with what the component asked for. own is the component's own cgroup path.
func (c config) checkSessionCgroup(root string, pid int, own string) error {
	path, pids, mem, swap, err := readCgroupOf(root, pid)
	if err != nil {
		return err
	}
	if path == own {
		return fmt.Errorf("the session is in the component's own cgroup %s", own)
	}
	if want := strconv.Itoa(c.sessionPids()); pids != want {
		return fmt.Errorf("pids.max=%q in %s, want %s", pids, path, want)
	}
	if want := strconv.FormatInt(int64(c.sessionMemMB())<<20, 10); mem != want {
		return fmt.Errorf("memory.max=%q in %s, want %s", mem, path, want)
	}
	if swap != "0" {
		return fmt.Errorf("memory.swap.max=%q in %s, want 0", swap, path)
	}
	return nil
}

func ownCgroupPath() string {
	p, _, _, _, _ := readCgroupOf("/sys/fs/cgroup", os.Getpid())
	return p
}

// probeCgroup runs a throw-away scope with the real properties and reads the limits back from inside it. The result is what /v1/health reports.
func probeCgroup(c config) cgSettings {
	s := cgSettings{uid: os.Getuid()}
	s.l = sessionLimits{Mode: c.cgroupMode(), State: "not enforced", Warnings: []string{}}
	if s.l.Mode == "off" {
		s.l.Warnings = append(s.l.Warnings, "TOTV_STREAM_CGROUP=off: sessions run without a cgroup of their own (threads and RAM bounded only by the component's cgroup, if any)")
		return s
	}
	fail := func(why string) cgSettings {
		s.l.Warnings = append(s.l.Warnings, "per-session cgroup limits NOT enforced: "+why+"; run the component under a unit with TasksMax=/MemoryMax= (contrib/totv-streaming.service) or fix systemd --user")
		return s
	}
	sr, err := exec.LookPath("systemd-run")
	if err != nil {
		return fail("systemd-run not found")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		return fail("sh not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sr, "--user", "--scope", "--quiet", "--collect", "--description=totv-streaming probe")
	for _, p := range c.cgroupProps() {
		cmd.Args = append(cmd.Args, "-p", p)
	}
	cmd.Args = append(cmd.Args, "--", sh, "-c", `p=$(cut -d: -f3 /proc/self/cgroup); cd /sys/fs/cgroup$p && cat pids.max memory.max memory.swap.max`)
	cmd.Env = userBusEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fail("systemd-run --user --scope did not finish within 15 s")
		}
		return fail("systemd-run --user --scope failed: " + strings.TrimSpace(string(out)))
	}
	f := strings.Fields(string(out))
	if len(f) != 3 || f[0] != strconv.Itoa(c.sessionPids()) || f[1] != strconv.FormatInt(int64(c.sessionMemMB())<<20, 10) || f[2] != "0" {
		return fail(fmt.Sprintf("the scope did not get the limits (read back %q)", strings.Join(f, " ")))
	}
	s.systemdRun = sr
	s.l.State, s.l.Method = "enforced", "systemd-run --user --scope"
	s.l.PidsMax, s.l.MemoryMB, s.l.SwapZero = c.sessionPids(), c.sessionMemMB(), true
	return s
}

// cgroupMismatchRefuses: what a session does when the kernel's view of its cgroup differs from the request. require refuses; auto also refuses once the
// start probe proved the scope works (limits.session.state == "enforced": a later mismatch is a failure, not an absent feature); auto only degrades at the probe.
func cgroupMismatchRefuses(mode, state string) bool {
	return mode == "require" || (mode == "auto" && state == "enforced")
}

func (c config) cgroupMode() string {
	switch c.CgroupMode {
	case "auto", "require":
		return c.CgroupMode
	}
	return "off"
}

func logCgroup(s cgSettings) {
	for _, w := range s.l.Warnings {
		log.Printf("WARNING: %s", w)
	}
	if s.l.State == "enforced" {
		log.Printf("per-session cgroup: enforced via %s (pids.max=%d memory.max=%d MiB memory.swap.max=0)", s.l.Method, s.l.PidsMax, s.l.MemoryMB)
	}
}
