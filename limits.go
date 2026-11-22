package main

// Per-session resource limits (hardening, round 2): free space checks before a download, the directory-size quota of the core's writable
// directory, the tmpfs check of TOTV_STREAM_RW_TMP and the clean-up of directories a crashed component left behind.

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	spaceSlack     = 64 << 20  // free space that must remain after the ROM is on disk
	defaultRWMax   = 512 << 20 // ceiling for what a session's writable directory may hold (TOTV_STREAM_RW_MAX_MB)
	defaultRWFiles = 20000     // ceiling for the number of entries (files, directories, links) in it (TOTV_STREAM_RW_MAX_FILES); a tmpfs without nr_inodes= has none
	defaultFileMax = 512 << 20 // RLIMIT_FSIZE of the child (TOTV_STREAM_MAX_FILE_MB): the largest single file it can create or grow
	rwMinFree      = 64 << 20  // free space the writable tmpfs must have for a session to start
	tmpfsHugeWarn  = 8 << 30   // a tmpfs this big has no practical limit: warn (mount it with size=)
)

var errNoSpace = errors.New("not enough free space")

// freeBytes is a variable so the tests can pretend a small disk.
var freeBytes = func(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// checkSpace fails with errNoSpace when dir has less than need bytes available.
func checkSpace(dir string, need int64) error {
	free, err := freeBytes(dir)
	if err != nil {
		return err
	}
	if need < 0 || free < uint64(need) {
		return errNoSpace
	}
	return nil
}

// tmpfsInfo reports whether dir is on a tmpfs and how big that tmpfs is.
func tmpfsInfo(dir string) (isTmpfs bool, size uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, 0, err
	}
	return st.Type == unix.TMPFS_MAGIC, st.Blocks * uint64(st.Bsize), nil
}

// checkRWTmp: the writable directory of a core is RAM-backed and bounded (README: TOTV_STREAM_RW_TMP is a tmpfs with size=), because a
// directory on a disk would be filled by a hostile core. Refused unless the operator explicitly allows a disk (development).
func (c config) checkRWTmp() error {
	if real, err := filepath.EvalSymlinks(c.RWTmp); err == nil && (real == "/dev/shm" || strings.HasPrefix(real, "/dev/shm/")) {
		// the PSP child mounts its own private tmpfs over /dev/shm (host option shm=1): a save directory below it would be hidden from the core
		return errors.New("TOTV_STREAM_RW_TMP must not be under /dev/shm (the core gets a private /dev/shm that hides it)")
	}
	ok, size, err := tmpfsInfo(c.RWTmp)
	if err != nil {
		return err
	}
	if !ok && !c.RWAllowDisk {
		return errors.New("TOTV_STREAM_RW_TMP is not a tmpfs (mount one with size=, or TOTV_STREAM_RW_TMP_ALLOW_DISK=1 for development)")
	}
	if ok && size > tmpfsHugeWarn {
		log.Printf("WARNING: TOTV_STREAM_RW_TMP is a tmpfs of %d GiB: mount it with a size= limit (the per-session quota is %d MiB)", size>>30, c.rwMax()>>20)
	}
	if err := checkSpace(c.RWTmp, rwMinFree); err != nil {
		return err
	}
	return nil
}

func (c config) rwMax() int64 {
	if c.RWMax > 0 {
		return c.RWMax
	}
	return defaultRWMax
}
func (c config) fileMax() int64 {
	if c.FileMax > 0 {
		return c.FileMax
	}
	return defaultFileMax
}

func (c config) rwFilesMax() int {
	if c.RWFiles > 0 {
		return c.RWFiles
	}
	return defaultRWFiles
}

// dirUsage adds up the bytes of every regular file below root and counts every entry (file, directory, link, anything: each one is an inode),
// never following a link. Empty files and directories cost no bytes but fill the tmpfs inode table, hence the count.
func dirUsage(root string) (bytes int64, entries int) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != root {
			entries++
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	return bytes, entries
}

var orphanRe = regexp.MustCompile(`^(ses|rw)-[0-9]+$`)

// cleanOrphans removes the session directories (ses-<digits>, rw-<digits>: what os.MkdirTemp makes) that a component killed in the middle of a
// session left in root. Run at start, when no session exists. Only real directories owned by this user; links and other names stay.
func cleanOrphans(root string) int {
	ents, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() || !orphanRe.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(root, e.Name())
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
			continue
		}
		if os.RemoveAll(p) == nil {
			n++
		}
	}
	return n
}

// okGPUDevice: a device node path for the host option string.
func okGPUDevice(d string) bool {
	return strings.HasPrefix(d, "/dev/") && !strings.ContainsAny(d, ";=") && filepath.Clean(d) == d && !strings.Contains(d, "..")
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// ---- cgroup limits (hardening, revision 3) ----
// The component cannot cap the processes or the memory of its children itself (no cgroup of its own without privileges), and RLIMIT_NPROC cannot do it
// (see the README). The seccomp filter forbids new processes; threads and memory are bounded only by the cgroup the operator puts the component in.

type cgroupLimits struct {
	PidsMax       string   `json:"pids_max"`   // the tightest pids.max found from the component's cgroup up to the root ("max" = none)
	MemoryMax     string   `json:"memory_max"` // idem for memory.max
	PidsLimited   bool     `json:"pids_limited"`
	MemoryLimited bool     `json:"memory_limited"`
	Warnings      []string `json:"warnings"`
}

// cgroupLimitsAt reads <root>/<cgroup of /proc/self/cgroup>/{pids.max,memory.max} and every ancestor up to root (a limit on a parent slice binds the
// children). root is /sys/fs/cgroup; selfCgroup is the content of /proc/self/cgroup (cgroup v2: one line "0::/path"). Tests pass their own.
func cgroupLimitsAt(root, selfCgroup string) cgroupLimits {
	l := cgroupLimits{PidsMax: "max", MemoryMax: "max", Warnings: []string{}}
	rel := ""
	for _, ln := range strings.Split(selfCgroup, "\n") {
		if strings.HasPrefix(ln, "0::") {
			rel = strings.TrimPrefix(ln, "0::")
		}
	}
	if rel == "" {
		l.Warnings = append(l.Warnings, "cgroup v2 not found: pids.max and memory.max cannot be checked; cap the component with the container's pids_limit/mem_limit or systemd TasksMax=/MemoryMax=")
		return l
	}
	rel = filepath.Clean("/" + rel)
	read := func(name string) (string, bool) { // tightest numeric value from the leaf up to root
		best, found := "max", false
		var bestN uint64
		for dir := rel; ; dir = filepath.Dir(dir) {
			if b, err := os.ReadFile(filepath.Join(root, dir, name)); err == nil {
				if v := strings.TrimSpace(string(b)); v != "" && v != "max" {
					if n, err := strconv.ParseUint(v, 10, 64); err == nil && (!found || n < bestN) {
						best, bestN, found = v, n, true
					}
				}
			}
			if dir == "/" {
				break
			}
		}
		return best, found
	}
	l.PidsMax, l.PidsLimited = read("pids.max")
	l.MemoryMax, l.MemoryLimited = read("memory.max")
	if _, err := os.Stat(root); err != nil {
		l.Warnings = append(l.Warnings, "/sys/fs/cgroup is not readable: pids.max and memory.max cannot be checked")
		return l
	}
	if !l.PidsLimited {
		l.Warnings = append(l.Warnings, "no pids.max on the component's cgroup (nor on a parent): a hostile core can create threads without a ceiling; set TasksMax= (systemd) or pids_limit (compose)")
	}
	if !l.MemoryLimited {
		l.Warnings = append(l.Warnings, "no memory.max on the component's cgroup (nor on a parent): a hostile core can use all RAM; set MemoryMax= (systemd) or mem_limit (compose)")
	}
	return l
}

func readCgroupLimits() cgroupLimits {
	self, _ := os.ReadFile("/proc/self/cgroup")
	return cgroupLimitsAt("/sys/fs/cgroup", string(self))
}
