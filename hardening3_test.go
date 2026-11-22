package main

// Hardening, revision 3: the F_SETOWN/O_ASYNC kill of the parent, IPC namespace, no new processes, cgroup limits, inode quota, PDEATHSIG.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The victim: a copy of this test binary playing the parent of retro-emu. It starts the host with a hostile core, waits for the hello, then writes
// input messages for a second, the way the real component does. If the core managed to arm its stdin to SIGKILL its parent, this process dies.
func TestVictimHelper(t *testing.T) {
	if os.Getenv("TOTV_VICTIM") != "1" {
		t.Skip("helper of TestAsyncOwnerAttackBlocked")
	}
	cmd := exec.Command(testEmuBin(), os.Getenv("TOTV_VICTIM_CORE"), os.Getenv("TOTV_VICTIM_ROM"), "-", os.Getenv("TOTV_VICTIM_SYS"), "640", "480", os.Getenv("TOTV_VICTIM_OPTS"))
	cmd.Env = strings.Fields(os.Getenv("TOTV_VICTIM_CHILDENV"))
	cmd.Stderr = os.Stdout
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	hello := make(chan struct{})
	go func() {
		var hdr [8]byte
		first := true
		for {
			if _, err := io.ReadFull(out, hdr[:]); err != nil {
				return
			}
			n := binary.LittleEndian.Uint32(hdr[4:])
			if _, err := io.CopyN(io.Discard, out, int64(n)); err != nil {
				return
			}
			if hdr[0] == 'H' && first {
				first = false
				close(hello)
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(10 * time.Second):
		t.Fatal("no hello")
	}
	for i := 0; i < 100; i++ {
		if _, err := in.Write([]byte{1, byte(i), 0, 0}); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _ = in.Write([]byte{2, 0, 0, 0})
	_ = in.Close()
	_ = cmd.Wait()
	os.Stdout.WriteString("VICTIM-ALIVE\n")
}

func runVictim(t *testing.T, childEnv, opts string) (string, error) {
	t.Helper()
	cwd, _ := os.Getwd()
	rom := filepath.Join(t.TempDir(), "rom.bin")
	_ = os.WriteFile(rom, bytes.Repeat([]byte{1}, 4096), 0o600)
	self, _ := os.Executable()
	c := exec.Command(self, "-test.run=^TestVictimHelper$", "-test.v")
	c.Env = append(os.Environ(), "TOTV_VICTIM=1", "TOTV_VICTIM_CORE="+filepath.Join(cwd, "bin", "attackcore_libretro.so"), "TOTV_VICTIM_ROM="+rom,
		"TOTV_VICTIM_SYS="+t.TempDir(), "TOTV_VICTIM_OPTS="+opts, "TOTV_VICTIM_CHILDENV="+childEnv)
	out, err := c.CombinedOutput()
	return string(out), err
}

// A-1: a core that points its stdin pipe at the parent with F_SETSIG=SIGKILL + O_ASYNC (before the sandbox, in a constructor, or inside it) must not
// be able to kill the parent. The Landlock signal scope is switched OFF (noscope=1) so that only the seccomp filter and the descriptor scrub are
// being tested: that is the situation of every kernel older than 6.12.
func TestAsyncOwnerAttackBlocked(t *testing.T) {
	for _, tc := range []struct{ name, env, wantLog string }{
		{"inside-sandbox", "ATTACK_LOAD=1", "attack(load): F_SETOWN=-1(1) F_SETSIG=-1(1) O_ASYNC=0(0) armed=0"},
		{"constructor", "ATTACK_CTOR=1", "attack(ctor): F_SETOWN=0(0) F_SETSIG=0(0) O_ASYNC=0(0) armed=1"}, // armed before the sandbox: the scrub must undo it
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runVictim(t, tc.env, "noscope=1")
			if !strings.Contains(out, tc.wantLog) {
				t.Errorf("the core's log is not what the test expects (%q):\n%s", tc.wantLog, out)
			}
			if err != nil || !strings.Contains(out, "VICTIM-ALIVE") {
				t.Fatalf("THE PARENT WAS KILLED (err %v):\n%s", err, out)
			}
		})
	}
}

func selftestJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, err := exec.Command(testEmuBin(), append([]string{"--selftest"}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if r["seccomp"] != true {
		t.Skipf("no seccomp here: %s", out)
	}
	return r
}

func TestSelftestHardening3(t *testing.T) {
	r := selftestJSON(t)
	for _, k := range []string{"async_owner_blocked", "sysv_unreachable", "fork_blocked", "thread_works"} {
		if r[k] != true {
			t.Errorf("%s = %v: %v", k, r[k], r)
		}
	}
	if r["netns"] == true && r["ipcns"] != true {
		t.Errorf("own user+net namespace but no IPC namespace: %v", r)
	}
	// a container seccomp profile that predates CLONE_NEWIPC: no ipc namespace, SysV IPC denied outright (EPERM), everything else as before
	r = selftestJSON(t, "noipc")
	if r["ipcns"] != false || r["sysv_unreachable"] != true || r["seccomp"] != true {
		t.Errorf("fallback without CLONE_NEWIPC: %v", r)
	}
}

// the real filter stays usable for what the cores need: plain threads, F_GETFL/F_SETFL(O_NONBLOCK) (proved in the selftest above) and a whole session
func TestSessionStillWorksWithNewFilter(t *testing.T) {
	e := newEnv(t, nil)
	e.create("new-filter")
}

// PDEATHSIG: when the component dies (here: SIGKILL, no cleanup at all) its emulator child dies with it, even when it is stuck inside the core.
func TestChildDiesWithParent(t *testing.T) {
	if os.Getenv("TOTV_SPAWNER") == "1" { // the "component": starts a child through the same code the sessions use, prints its pid and sleeps
		cwd, _ := os.Getwd()
		rom := filepath.Join(t.TempDir(), "rom.bin")
		_ = os.WriteFile(rom, bytes.Repeat([]byte{1}, 4096), 0o600)
		cmd := exec.Command(testEmuBin(), filepath.Join(cwd, "bin", "testcore_libretro.so"), rom, "-", t.TempDir(), "640", "480")
		cmd.Env = []string{}
		if _, err := cmd.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.StdoutPipe(); err != nil {
			t.Fatal(err)
		}
		if err := startChild(cmd); err != nil {
			t.Fatal(err)
		}
		os.Stdout.WriteString("CHILD " + itoa64(int64(cmd.Process.Pid)) + "\n")
		time.Sleep(time.Minute)
		return
	}
	self, _ := os.Executable()
	c := exec.Command(self, "-test.run=^TestChildDiesWithParent$")
	c.Env = append(os.Environ(), "TOTV_SPAWNER=1")
	stdout, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	buf := make([]byte, 4096)
	var got string
	for pid == 0 {
		n, err := stdout.Read(buf)
		got += string(buf[:n])
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "CHILD ") {
				pid = int(mustAtoi(strings.TrimSpace(l[6:])))
			}
		}
		if err != nil {
			break
		}
	}
	if pid == 0 {
		t.Fatalf("no child pid: %q", got)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("child is not running")
	}
	time.Sleep(300 * time.Millisecond)
	_ = c.Process.Kill()
	_, _ = c.Process.Wait()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if b, err := os.ReadFile("/proc/" + itoa64(int64(pid)) + "/stat"); err != nil || strings.Contains(string(b), ") Z ") {
			return // gone (or a zombie waiting for init)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the emulator child %d survived the death of its parent", pid)
}

func mustAtoi(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func TestCgroupLimits(t *testing.T) {
	root := t.TempDir()
	write := func(rel, pids, mem string) {
		d := filepath.Join(root, rel)
		_ = os.MkdirAll(d, 0o755)
		if pids != "" {
			_ = os.WriteFile(filepath.Join(d, "pids.max"), []byte(pids+"\n"), 0o644)
		}
		if mem != "" {
			_ = os.WriteFile(filepath.Join(d, "memory.max"), []byte(mem+"\n"), 0o644)
		}
	}
	// nothing set anywhere: both warned
	write("a/b", "max", "max")
	write("a", "max", "max")
	write("", "", "")
	l := cgroupLimitsAt(root, "0::/a/b\n")
	if l.PidsLimited || l.MemoryLimited || len(l.Warnings) != 2 {
		t.Errorf("unlimited: %+v", l)
	}
	// a limit on an ANCESTOR counts (systemd slice), the leaf says max
	write("a", "512", "max")
	l = cgroupLimitsAt(root, "0::/a/b\n")
	if !l.PidsLimited || l.PidsMax != "512" || l.MemoryLimited || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "memory.max") {
		t.Errorf("ancestor pids: %+v", l)
	}
	// the tightest limit wins
	write("a/b", "100", "2147483648")
	l = cgroupLimitsAt(root, "0::/a/b\n")
	if l.PidsMax != "100" || l.MemoryMax != "2147483648" || len(l.Warnings) != 0 {
		t.Errorf("both: %+v", l)
	}
	// no cgroup v2 (or unreadable): says so instead of pretending
	l = cgroupLimitsAt(filepath.Join(root, "nope"), "0::/a/b\n")
	if l.PidsLimited || l.MemoryLimited || len(l.Warnings) == 0 {
		t.Errorf("no cgroup: %+v", l)
	}
}

func TestHealthReportsLimitsAndIsolation(t *testing.T) {
	e := newEnv(t, nil)
	code, _, body := e.call("GET", "/v1/health", nil, testSecret)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var h struct {
		Isolation map[string]any `json:"isolation"`
		Limits    map[string]any `json:"limits"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"landlock_abi", "landlock_scope", "async_owner_blocked", "ipcns", "fork_blocked"} {
		if _, ok := h.Isolation[k]; !ok {
			t.Errorf("isolation.%s missing: %s", k, body)
		}
	}
	for _, k := range []string{"pids_max", "memory_max", "pids_limited", "memory_limited", "warnings"} {
		if _, ok := h.Limits[k]; !ok {
			t.Errorf("limits.%s missing: %s", k, body)
		}
	}
}

func TestDirUsageCountsEntries(t *testing.T) {
	d := t.TempDir()
	_ = os.MkdirAll(filepath.Join(d, "a/b/c"), 0o700)
	for i := 0; i < 10; i++ {
		_ = os.WriteFile(filepath.Join(d, "a/b", "f"+itoa64(int64(i))), nil, 0o600) // empty files: zero bytes, ten inodes
	}
	_ = os.Symlink("/etc/passwd", filepath.Join(d, "lnk"))
	bytes, entries := dirUsage(d)
	if bytes != 0 || entries != 14 { // a, a/b, a/b/c, 10 files, 1 link
		t.Errorf("bytes %d entries %d", bytes, entries)
	}
}
