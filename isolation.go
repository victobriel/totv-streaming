package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// checkProcStatus validates the content of /proc/<pid>/status: the child must be under a seccomp filter
// ("Seccomp: 2") and have no_new_privs set ("NoNewPrivs: 1").
func checkProcStatus(status string) error {
	var seccomp, nnp string
	for _, line := range strings.Split(status, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			switch k {
			case "Seccomp":
				seccomp = strings.TrimSpace(v)
			case "NoNewPrivs":
				nnp = strings.TrimSpace(v)
			}
		}
	}
	if seccomp != "2" {
		return fmt.Errorf("Seccomp=%q, want 2", seccomp)
	}
	if nnp != "1" {
		return fmt.Errorf("NoNewPrivs=%q, want 1", nnp)
	}
	return nil
}

// verifyChildSandbox asks the kernel, not the child, whether the filter is installed. The child sends its hello only
// after applying the sandbox, so at that point the state is final (the filter is installed with TSYNC and never removed).
func verifyChildSandbox(pid int) error {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return err
	}
	if err := checkProcStatus(string(b)); err != nil {
		return err
	}
	// threads too: a GPU driver may start helper threads before the sandbox (the filter is installed with TSYNC, which
	// covers them; Landlock does not, see the README), and the core may start its own after it.
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/status", pid))
	for _, tf := range tasks {
		tb, err := os.ReadFile(tf)
		if err != nil {
			continue // the thread ended between the glob and the read
		}
		if err := checkProcStatus(string(tb)); err != nil {
			return fmt.Errorf("%s: %w", tf, err)
		}
	}
	return nil
}

// childHasOwnNetns is true when the child's network namespace is not ours (verified from outside via /proc).
func childHasOwnNetns(pid int) bool {
	mine, err1 := os.Readlink("/proc/self/ns/net")
	theirs, err2 := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
	return err1 == nil && err2 == nil && mine != theirs
}

// ---- the process tree of a session (PID namespace, GPU isolation option 2) ----
// retro-emu forks once after unshare(CLONE_NEWPID): the process the component started is a tiny supervisor OUTSIDE the namespace and its only child is
// the emulator, pid 1 INSIDE it (emu/host.c, enter_pidns). Everything that must describe the emulator (Seccomp, NoNewPrivs, descriptors, threads,
// CPU, signals for the stall test) has to look at the emulator, not at the supervisor: corePID finds it with the kernel's own record of children.

// childrenOf lists the direct children of pid (needs CONFIG_PROC_CHILDREN; nil when unavailable).
func childrenOf(pid int) []int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// nsPID reads the NSpid line of /proc/<pid>/status: the pid in every namespace from the outermost to the innermost.
func nsPID(status string) []int {
	for _, line := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(line, "NSpid:"); ok {
			var out []int
			for _, f := range strings.Fields(v) {
				if n, err := strconv.Atoi(f); err == nil {
					out = append(out, n)
				}
			}
			return out
		}
	}
	return nil
}

// corePID: the emulator for the process the component started. With a PID namespace it is the supervisor's single child, whose pid inside the
// namespace is 1; without one (old kernel, profile that refuses CLONE_NEWPID, ...) the started process is the emulator itself.
func corePID(started int) int {
	for _, c := range childrenOf(started) {
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", c)); err == nil {
			if ns := nsPID(string(b)); len(ns) >= 2 && ns[len(ns)-1] == 1 {
				return c
			}
		}
	}
	return started
}

// childInOwnPIDNS: from outside, the emulator is pid 1 of a PID namespace that is not ours (/proc/<pid>/ns/pid differs).
func childInOwnPIDNS(started int) bool {
	core := corePID(started)
	if core == started {
		return false
	}
	mine, err1 := os.Readlink("/proc/self/ns/pid")
	theirs, err2 := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", core))
	return err1 == nil && err2 == nil && mine != theirs
}

// ---- descriptors a GPU session may hold (GPU isolation, option 2) ----

// forbiddenGPUFD: device nodes the emulator must NOT hold after the sandbox. Only /dev/nvidiactl stays (see README "GPU isolation (opção 2)"): the
// device node /dev/nvidia<N>, NVKMS (/dev/nvidia-modeset), UVM, the capability nodes and the DRM card (modesetting) are closed by the host.
// The DRM render node is not on the list: Mesa drivers need it open.
func forbiddenGPUFD(target string) bool {
	if t, ok := strings.CutPrefix(target, "/dev/nvidia"); ok {
		return t != "ctl"
	}
	return strings.HasPrefix(target, "/dev/dri/card") || strings.HasPrefix(target, "/dev/nvidia-caps/")
}

// gpuFDViolations lists the descriptors of pid that point at a forbidden node.
func gpuFDViolations(pid int) []string {
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil { // cannot prove the surface: that is a violation, not a pass
		return []string{fmt.Sprintf("<cannot list /proc/%d/fd: %v>", pid, err)}
	}
	var bad []string
	for _, e := range ents {
		if t, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, e.Name())); err == nil && forbiddenGPUFD(t) {
			bad = append(bad, e.Name()+"="+t)
		}
	}
	return bad
}
