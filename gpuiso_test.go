// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

// GPU isolation, option 2: minimal descriptor surface, PID namespace, per-session cgroup.

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestForbiddenGPUFD(t *testing.T) {
	for tgt, want := range map[string]bool{
		"/dev/nvidia0": true, "/dev/nvidia1": true, "/dev/nvidia-modeset": true, "/dev/nvidia-uvm": true, "/dev/nvidia-uvm-tools": true,
		"/dev/nvidia-caps/nvidia-cap1": true, "/dev/dri/card1": true,
		"/dev/nvidiactl": false, "/dev/dri/renderD128": false, "/dev/null": false, "pipe:[1]": false, "/usr/lib/libEGL.so.1": false,
	} {
		if got := forbiddenGPUFD(tgt); got != want {
			t.Errorf("forbiddenGPUFD(%q)=%v want %v", tgt, got, want)
		}
	}
}

func TestNSPid(t *testing.T) {
	if got := nsPID("Name:\tx\nNSpid:\t4242\t1\nNSsid:\t1\n"); len(got) != 2 || got[0] != 4242 || got[1] != 1 {
		t.Fatalf("nsPID=%v", got)
	}
	if nsPID("Name:\tx\n") != nil {
		t.Fatal("no NSpid line must give nil")
	}
}

// startHost runs retro-emu directly on a core, waits for the hello and returns the process we started (the supervisor when a PID namespace is used)
// and a stop function. opts is the 8th argument.
func startHost(t *testing.T, core, opts string) (*exec.Cmd, uint32, func()) {
	t.Helper()
	dir := t.TempDir()
	romp := filepath.Join(dir, "game.bin")
	os.WriteFile(romp, []byte("x"), 0o600)
	args := []string{filepath.Join(filepath.Dir(testEmuBin()), core+"_libretro.so"), romp, "-", filepath.Join(dir, "sys"), "320", "240"}
	if opts != "" {
		args = append(args, opts)
	}
	cmd := exec.Command(testEmuBin(), args...)
	cmd.Env = []string{}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := startChild(cmd); err != nil {
		t.Fatal(err)
	}
	stop := func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }
	var hdr [8]byte
	var payload []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.ReadFull(out, hdr[:]); err == nil {
			payload = make([]byte, binary.LittleEndian.Uint32(hdr[4:]))
			io.ReadFull(out, payload)
			go io.Copy(io.Discard, out)
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		stop()
		t.Fatal("child sent nothing")
	}
	if hdr[0] != 'H' || len(payload) < 32 {
		stop()
		t.Fatalf("no hello: %q %q", hdr[0], payload)
	}
	return cmd, binary.LittleEndian.Uint32(payload[28:32]), stop
}

// TestGPUDescriptorSurface: the control (gfd=keep) shows that the driver does open the dangerous nodes during the context creation, so the
// absence in the default run is the host's doing, not a property of this machine; the default run holds only /dev/nvidiactl of the GPU nodes.
func TestGPUDescriptorSurface(t *testing.T) {
	g := cachedGPU(testEmuBin())
	if !g.OK || !strings.Contains(g.Version, "NVIDIA") {
		t.Skipf("needs the NVIDIA driver (retro-emu --gltest: %+v)", g)
	}
	opts := "gl=core33;dev=/dev/nvidiactl"

	cmd, fl, stop := startHost(t, "glcore", opts+";gfd=keep")
	keep := fdTargets(corePID(cmd.Process.Pid))
	stop()
	t.Logf("gfd=keep   flags=%d fds: %v", fl, keep)
	if fl&256 != 0 {
		t.Errorf("gfd=keep must not report closed descriptors (flags %d)", fl)
	}
	if countFD(keep, "/dev/nvidia0") == 0 && countFD(keep, "/dev/nvidia-modeset") == 0 {
		t.Skip("this driver does not open /dev/nvidia0 or /dev/nvidia-modeset in the first place: nothing to compare")
	}

	cmd, fl, stop = startHost(t, "glcore", opts)
	defer stop()
	pid := corePID(cmd.Process.Pid)
	min := fdTargets(pid)
	t.Logf("default    flags=%d fds: %v", fl, min)
	if fl&256 == 0 {
		t.Errorf("flags %d: closing not reported", fl)
	}
	for _, f := range min {
		if forbiddenGPUFD(f) || strings.HasPrefix(f, "/dev/dri/") {
			t.Errorf("still open in the emulator: %s", f)
		}
	}
	if len(gpuFDViolations(pid)) != 0 {
		t.Errorf("violations: %v", gpuFDViolations(pid))
	}
	// the closed nodes do not come back: Landlock allows only /dev/nvidiactl after the sandbox, so the core cannot open them again
	if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/0", pid)); err != nil { // sanity: /proc of the emulator is readable from here
		t.Log("fdinfo:", err)
	}
	// the numbers are reserved by /dev/null (dup3), not freed: every descriptor that pointed at a closed node now points at /dev/null, so the driver's
	// stale number cannot hit a file opened later (ISO, save, shm)
	closedKeep := 0
	for _, f := range keep {
		if f != "/dev/nvidiactl" && (strings.HasPrefix(f, "/dev/nvidia") || strings.HasPrefix(f, "/dev/dri/")) {
			closedKeep++
		}
	}
	if closedKeep == 0 {
		t.Errorf("gfd=keep holds no node that the default run must close: nothing to compare")
	}
	if nn := countFD(min, "/dev/null"); nn < closedKeep {
		t.Errorf("default run: %d descriptors on /dev/null, but gfd=keep held %d closed-able GPU descriptors: the numbers were not reserved", nn, closedKeep)
	}
	if len(min) < len(keep) {
		t.Errorf("default run holds %d descriptors, gfd=keep %d: a number was freed instead of reserved", len(min), len(keep))
	}
}

// TestPIDNamespaceChain: the process we start is a supervisor, its only child is the emulator = pid 1 of a namespace that is not ours; killing the
// supervisor (what the component does on a timeout, and what its own death does through Pdeathsig) takes the emulator with it.
func TestPIDNamespaceChain(t *testing.T) {
	if !probeIsolation(testEmuBin()).PIDNS {
		t.Skip("no PID namespaces here")
	}
	cmd, fl, stop := startHost(t, "testcore", "")
	defer stop()
	if fl&flagPIDNS == 0 {
		t.Fatalf("flags %d: no PID namespace", fl)
	}
	sup := cmd.Process.Pid
	core := corePID(sup)
	if core == sup {
		t.Fatal("corePID found no child")
	}
	if kids := childrenOf(sup); len(kids) != 1 {
		t.Fatalf("supervisor children %v, want exactly the emulator", kids)
	}
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", core))
	if ns := nsPID(string(b)); len(ns) < 2 || ns[len(ns)-1] != 1 {
		t.Fatalf("NSpid=%v: the emulator is not pid 1 of its namespace", ns)
	}
	if !childInOwnPIDNS(sup) {
		t.Fatal("/proc/<pid>/ns/pid is ours")
	}
	// the supervisor is under the same filter and holds nothing but 0,1,2 (stdin/dev/null)
	if err := verifyChildSandbox(sup); err != nil {
		t.Errorf("supervisor: %v", err)
	}
	if err := verifyChildSandbox(core); err != nil {
		t.Errorf("emulator: %v", err)
	}
	if fds := fdTargets(sup); len(fds) > 4 {
		t.Errorf("supervisor holds %v", fds)
	}
	syscall.Kill(sup, syscall.SIGKILL)
	waitFor(t, "emulator dies with the supervisor", 3*time.Second, func() bool {
		_, err := os.Stat(fmt.Sprintf("/proc/%d", core))
		if err != nil {
			return true
		}
		st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", core))
		return strings.Contains(string(st), ") Z ") // zombie awaiting its (dead) reaper
	})
}

// ---- per-session cgroup ----

func cgEnv(t *testing.T, pids, memMB int) *testEnv {
	t.Helper()
	e := newEnv(t, func(c *config) { c.CgroupMode, c.SessionPids, c.SessionMemMB = "require", pids, memMB })
	if e.srv.cg.l.State != "enforced" {
		t.Skipf("no usable systemd --user scope here: %v", e.srv.cg.l.Warnings)
	}
	return e
}

func TestSessionCgroupEnforced(t *testing.T) {
	e := cgEnv(t, 96, 512)
	e.create("cg1")
	ss := e.srv.get("cg1")
	core := ss.corePID()
	path, pids, mem, swap, err := readCgroupOf("/sys/fs/cgroup", core)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session cgroup %s pids.max=%s memory.max=%s memory.swap.max=%s", path, pids, mem, swap)
	if pids != "96" || mem != fmt.Sprint(512<<20) || swap != "0" {
		t.Fatalf("limits not as configured: %s %s %s", pids, mem, swap)
	}
	if path == ownCgroupPath() || !strings.HasPrefix(path, "/user.slice/user-") {
		t.Fatalf("cgroup %s is not a scope under the user's own subtree (own: %s)", path, ownCgroupPath())
	}
	// the emulator's environment is empty: `env -i` removed what systemd-run needed
	if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", core)); len(b) != 0 {
		t.Errorf("the emulator got an environment: %q", b)
	}
	code, m, _ := e.call("GET", "/v1/health", nil, testSecret)
	lim, _ := m["limits"].(map[string]any)
	sess, _ := lim["session"].(map[string]any)
	if code != 200 || sess["state"] != "enforced" || sess["sessions_verified"] != float64(1) {
		t.Fatalf("health limits.session = %v", sess)
	}
	t.Logf("health limits.session = %v", sess)
	// a PID namespace chain inside the scope: supervisor and emulator are in the same cgroup
	if ss.cmd.Process.Pid != core {
		sp, _, _, _, _ := readCgroupOf("/sys/fs/cgroup", ss.cmd.Process.Pid)
		if sp != path {
			t.Errorf("supervisor in %s, emulator in %s", sp, path)
		}
	}
	e.call("DELETE", "/v1/sessions/cg1", nil, testSecret)
}

func TestCgroupModeOffReportsNotEnforced(t *testing.T) {
	e := newEnv(t, func(c *config) { c.CgroupMode = "off" })
	code, m, _ := e.call("GET", "/v1/health", nil, testSecret)
	lim, _ := m["limits"].(map[string]any)
	sess, _ := lim["session"].(map[string]any)
	if code != 200 || sess["state"] != "not enforced" || sess["mode"] != "off" {
		t.Fatalf("limits.session = %v", sess)
	}
	if w, _ := sess["warnings"].([]any); len(w) == 0 {
		t.Fatal("not enforced without a warning")
	}
}

func TestCgroupRequireRefusesWhenUnavailable(t *testing.T) {
	e := newEnv(t, func(c *config) { c.CgroupMode = "require" })
	e.srv.cg = cgSettings{l: sessionLimits{Mode: "require", State: "not enforced", Warnings: []string{"test"}}} // pretend the probe failed
	body := e.createBody("cgx")
	code, m, _ := e.call("POST", "/v1/sessions", body, testSecret)
	if code == 201 {
		t.Fatalf("a session started without the cgroup in require mode: %v", m)
	}
}

// cgroupVictim runs a hostile GL core in a session with small limits and returns what the kernel recorded.
func TestCgroupThreadBomb(t *testing.T) {
	e := cgEnv(t, 64, 1024)
	skipWithoutGPU(t, e)
	body := e.createBody("tb")
	body["platform"], body["core"] = "n64", "server_glhogt"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	ss := e.srv.get("tb")
	path, _, _, _, _ := readCgroupOf("/sys/fs/cgroup", ss.corePID())
	p := newTestPeer(t)
	defer p.pc.Close()
	p.negotiate(t, e, "tb")
	// the core starts the bomb one second after the first frame (60 frames)
	var cur, events string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(400 * time.Millisecond)
		b1, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "pids.current"))
		b2, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "pids.events"))
		cur, events = strings.TrimSpace(string(b1)), strings.TrimSpace(string(b2))
		if strings.Contains(events, "max 1") || strings.Contains(events, "max 2") || (strings.Contains(events, "max ") && !strings.Contains(events, "max 0")) {
			break
		}
	}
	t.Logf("pids.current=%s pids.events=%q", cur, strings.ReplaceAll(events, "\n", " "))
	if strings.Contains(events, "max 0") || events == "" {
		t.Fatalf("the thread bomb never hit pids.max (events %q): did the core start it?", events)
	}
	var n int
	fmt.Sscan(cur, &n)
	if n > 64 || n < 60 {
		t.Fatalf("pids.current=%d, want a value just under pids.max=64", n)
	}
	// the component is fine and so is a second session
	if code, _, _ := e.call("GET", "/v1/health", nil, testSecret); code != 200 {
		t.Fatalf("health after the bomb: %d", code)
	}
	e.call("DELETE", "/v1/sessions/tb", nil, testSecret)
	e.create("after")
	e.call("DELETE", "/v1/sessions/after", nil, testSecret)
}

func TestCgroupMemoryHog(t *testing.T) {
	e := cgEnv(t, 128, 400)
	skipWithoutGPU(t, e)
	body := e.createBody("mh")
	body["platform"], body["core"] = "n64", "server_glhogm"
	if code, m, _ := e.call("POST", "/v1/sessions", body, testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	ss := e.srv.get("mh")
	core := ss.corePID()
	path, _, _, _, _ := readCgroupOf("/sys/fs/cgroup", core)
	p := newTestPeer(t)
	defer p.pc.Close()
	p.negotiate(t, e, "mh")
	var last string // memory.events as last seen before the scope disappeared
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "memory.events")); err == nil {
			last = strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " ")
		}
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", core))
		if err != nil || strings.Contains(string(st), ") Z ") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("the hog was not killed")
	}
	t.Logf("memory.events last seen: %q", last)
	t.Logf("cgroup %s: hog killed", path)
	if code, _, _ := e.call("GET", "/v1/health", nil, testSecret); code != 200 {
		t.Fatalf("component down after the memory hog")
	}
	waitFor(t, "the session is over", 10*time.Second, func() bool {
		select {
		case <-ss.exited:
			return true
		default:
			return e.srv.get("mh") == nil
		}
	})
	e.create("after")
	e.call("DELETE", "/v1/sessions/after", nil, testSecret)
}

func TestCheckSessionCgroupReadsBack(t *testing.T) {
	c := config{SessionPids: 64, SessionMemMB: 100}
	root := t.TempDir()
	self, _ := os.ReadFile("/proc/self/cgroup")
	if !strings.Contains(string(self), "0::") {
		t.Skip("no cgroup v2")
	}
	path, _, _, _, _ := readCgroupOf("/nonexistent", os.Getpid())
	dir := filepath.Join(root, path)
	os.MkdirAll(dir, 0o755)
	put := func(p, m, s string) {
		os.WriteFile(filepath.Join(dir, "pids.max"), []byte(p+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "memory.max"), []byte(m+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte(s+"\n"), 0o644)
	}
	put("64", fmt.Sprint(100<<20), "0")
	if err := c.checkSessionCgroup(root, os.Getpid(), "/some/other/cgroup"); err != nil {
		t.Fatal(err)
	}
	if err := c.checkSessionCgroup(root, os.Getpid(), path); err == nil {
		t.Fatal("a session in the component's own cgroup passed")
	}
	for _, bad := range [][3]string{{"max", fmt.Sprint(100 << 20), "0"}, {"64", "max", "0"}, {"64", fmt.Sprint(100 << 20), "max"}, {"65", fmt.Sprint(100 << 20), "0"}} {
		put(bad[0], bad[1], bad[2])
		if err := c.checkSessionCgroup(root, os.Getpid(), "/x"); err == nil {
			t.Errorf("limits %v accepted", bad)
		}
	}
}

func TestWrapCgroupClearsTheEnvironment(t *testing.T) {
	c := config{SessionPids: 77, SessionMemMB: 123}
	cmd := exec.Command("/opt/x/retro-emu", "core.so", "rom", "-", "/sys", "640", "480", "gl=core46")
	cmd.Env = []string{"RETRO_EMU_LOG=1"}
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env in PATH")
	}
	c.wrapCgroup(cmd, "/usr/bin/systemd-run", "ses 1/../x")
	got := strings.Join(cmd.Args, " ")
	for _, w := range []string{"systemd-run --user --scope", "TasksMax=77", "MemoryMax=123M", "MemorySwapMax=0", "-- " + envBin + " -i -- RETRO_EMU_LOG=1 /opt/x/retro-emu core.so rom - /sys 640 480 gl=core46"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %s", w, got)
		}
	}
	if strings.Contains(got, "ses 1/../x") {
		t.Errorf("unsanitised session id in the description: %s", got)
	}
	for _, e := range cmd.Env {
		if strings.Contains(strings.ToLower(e), "token") || strings.Contains(strings.ToLower(e), "secret") {
			t.Errorf("secret-looking variable passed: %s", e)
		}
	}
}

func TestCgroupMismatchRefuses(t *testing.T) {
	for _, tc := range []struct {
		mode, state string
		want        bool
	}{{"require", "enforced", true}, {"require", "not enforced", true}, {"auto", "enforced", true}, {"auto", "not enforced", false}, {"off", "enforced", false}, {"off", "not enforced", false}} {
		if got := cgroupMismatchRefuses(tc.mode, tc.state); got != tc.want {
			t.Errorf("mode=%s state=%s: refuses=%v want %v", tc.mode, tc.state, got, tc.want)
		}
	}
}

// the descriptor check must fail closed: a /proc/<pid>/fd that cannot be listed is a violation, not a pass
func TestGPUFDViolationsFailClosed(t *testing.T) {
	if v := gpuFDViolations(1 << 30); len(v) == 0 {
		t.Fatal("an unreadable /proc/<pid>/fd passed the GPU descriptor check")
	}
}

// TOTV_STREAM_PIDNS=require: a session whose host did not enter a PID namespace is refused (here: the host is told not to, pidns=0 via the probe flag)
func TestPIDNSRequireRefusesWithoutNamespace(t *testing.T) {
	e := newEnv(t, func(c *config) { c.PIDNSMode = "require"; c.AllowNoIsolation = true })
	if !e.srv.iso.PIDNS {
		t.Skip("this host cannot enter a PID namespace")
	}
	e.srv.iso.PIDNS = false // as if the kernel/profile refused CLONE_NEWPID: the child then reports no namespace
	e.srv.cfg.PIDNSMode = "require"
	if code, m, _ := e.call("POST", "/v1/sessions", e.createBody("pr"), testSecret); code == 201 {
		t.Fatalf("a session started in require mode although the PID namespace was not proved: %v", m)
	}
}
