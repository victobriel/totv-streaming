package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func testEmuBin() string {
	if p := os.Getenv("RETRO_EMU_TEST_BIN"); p != "" { // a binary built from an older host.c, to show that a test catches the old behaviour
		return p
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "bin", "retro-emu")
}

// TestSelftestEverything: the binary applies the sandbox to itself and proves, from inside, each denial.
func TestSelftestEverything(t *testing.T) {
	out, err := exec.Command(testEmuBin(), "--selftest").Output()
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if r["seccomp"] != true || r["landlock"] != true {
		t.Skipf("kernel without seccomp/landlock: %s", out)
	}
	for _, k := range []string{"udp_blocked", "passwd_blocked", "io_uring_blocked", "kill_other_blocked", "x32_killed",
		"sigqueue_other_blocked", "prlimit_other_blocked", "priority_other_blocked", "sched_other_blocked", "ioprio_other_blocked", "self_allowed",
		"unshare_blocked", "clone_newns_blocked", "clone3_enosys", "fork_works"} {
		if r[k] != true {
			t.Errorf("%s = %v: %s", k, r[k], out)
		}
	}
	if abi, _ := r["landlock_abi"].(float64); abi >= 6 && r["landlock_scope"] != true {
		t.Errorf("Landlock ABI %v but the signal scope did not hold: %s", abi, out)
	}
	if r["netns"] != true {
		t.Logf("netns false here (no unprivileged user namespaces): %s", out)
	}
}

func TestCheckProcStatus(t *testing.T) {
	good := "Name:\tx\nNoNewPrivs:\t1\nSeccomp:\t2\nSeccomp_filters:\t1\n"
	if err := checkProcStatus(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"NoNewPrivs:\t1\nSeccomp:\t0\n", "NoNewPrivs:\t1\nSeccomp:\t1\n", "NoNewPrivs:\t0\nSeccomp:\t2\n", "", "Seccomp:\t2\n"} {
		if checkProcStatus(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// this test process is not sandboxed: the check must say so
	if verifyChildSandbox(os.Getpid()) == nil {
		t.Fatal("an unsandboxed process passed the check")
	}
}

// TestParentChecksKernelView: the session is only "ready" after /proc says Seccomp 2 + NoNewPrivs 1 for the child.
func TestParentChecksKernelView(t *testing.T) {
	e := newEnv(t, nil)
	e.create("k1")
	ss := e.srv.get("k1")
	if ss.flags&3 != 3 {
		t.Skip("kernel without full sandbox")
	}
	if err := verifyChildSandbox(ss.corePID()); err != nil {
		t.Fatal(err)
	}
	if ss.netns != childHasOwnNetns(ss.corePID()) {
		t.Fatal("session netns disagrees with /proc")
	}
}

// runChild starts retro-emu on the test core directly and returns the first message type it sends.
func runChild(t *testing.T, rom []byte) (byte, string) {
	t.Helper()
	dir := t.TempDir()
	romp := filepath.Join(dir, "game.bin")
	os.WriteFile(romp, rom, 0o600)
	cmd := exec.Command(testEmuBin(), filepath.Join(filepath.Dir(testEmuBin()), "testcore_libretro.so"), romp, "-", filepath.Join(dir, "sys"), "320", "240")
	cmd.Env = []string{}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	var hdr [8]byte
	done := make(chan struct{})
	var payload []byte
	go func() {
		defer close(done)
		if _, err := io.ReadFull(out, hdr[:]); err == nil {
			payload = make([]byte, binary.LittleEndian.Uint32(hdr[4:]))
			io.ReadFull(out, payload)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child sent nothing")
	}
	return hdr[0], string(payload)
}

// TestCoreRunsOnlyInsideSandbox: dlopen (constructor) still sees /etc/passwd, while retro_load_game (after the
// sandbox) cannot open it, create a UDP socket or ptrace. The probe core fails to load otherwise.
func TestCoreRunsOnlyInsideSandbox(t *testing.T) {
	if !probeIsolation(testEmuBin()).Landlock {
		t.Skip("no landlock")
	}
	if k, p := runChild(t, append([]byte("SBXPROBE"), make([]byte, 64)...)); k != 'H' {
		t.Fatalf("probe failed: %q %q (core code ran outside the sandbox, or the sandbox is incomplete)", k, p)
	}
	if k, _ := runChild(t, bytes.Repeat([]byte{1}, 64)); k != 'H' {
		t.Fatalf("normal ROM did not load, kind %q", k)
	}
}
