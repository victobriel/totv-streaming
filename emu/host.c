/* SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (C) 2026 victobriel */
/* retro-emu: libretro host, child process of totv-streaming (GPL-3.0-or-later).
 *
 * Runs ONE core + ONE game. Talks to the parent only through stdin/stdout pipes:
 *   parent -> child (stdin), fixed 4-byte messages: [kind, a, b, c]
 *       kind 1 (input): a|b<<8 = 16-bit retropad mask, c = port (0..3)
 *       kind 2 (quit) : save RAM is sent, then the child exits
 *       kind 3 (axis) : a = port (0..3), b = axis (0 left X, 1 left Y, 2 right X, 3 right Y), c = int8 (x256 -> libretro int16)
 *   child -> parent (protocol pipe, a private copy of fd 1), frames: [type u8][3 pad][len u32 LE][payload]
 *       'H' hello: f64 fps, f64 sample_rate, u32 w, u32 h, u32 sram_size, u32 sandbox_flags (16 = GPU context made before the sandbox, 32 = private /dev/shm in its own mount namespace)
 *       'V' video: I420 w*h*3/2 bytes (scaled, nearest or GPU-blitted, to the fixed output size)
 *       'A' audio: s16le stereo at the core sample rate
 *       'S' save RAM
 *       'T' stage timing, 12 x float32 every 120 frames: avg core, blit, readback, convert, total; max of the same five; GPU span; 0
 *       'E' error text, 'X' bye
 *   usage: retro-emu <core.so> <rom> <sram|-> <system_dir> <out_w> <out_h> [opts]   (opts: "k=v;k=v", see main(): shmmb=<MiB> (private /dev/shm size, default 512), fsize=<bytes> (RLIMIT_FSIZE, default 512 MiB), save=/dir (the one writable dir), ro=/dir (extra read-only dir), gl=core46|core33|gl33,
 *          dev=/dev/<node> (Landlock exception), rb=sync|pbo, filter=nearest|linear, v.<core option>=<value>)
 *          retro-emu --selftest | --gltest
 * Order matters: the ROM and the save are read into memory and the core is dlopen()ed FIRST (no core code runs yet);
 * then the sandbox (user+net namespace, seccomp, Landlock) is applied; only after that retro_init/retro_load_game run,
 * so the core's code never executes outside the sandbox. A core with need_fullpath (PPSSPP) is not read into memory: it is
 * handed the path of a directory that holds only the ROM (readable, read-only), and may get ONE writable directory (save=), extra
 * read-only directories (ro=) and a private /dev/shm (shm=1). Sockets inherited from drivers are closed right before the sandbox.
 * It has no token, no network (seccomp + landlock net) and no disk access once the sandbox is applied.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/audit.h>
#include <linux/capability.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <libgen.h>
#include <poll.h>
#include <sched.h>
#include <sys/resource.h>
#include <signal.h>
#include <sys/wait.h>
#include <poll.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/shm.h>
#include <sys/ioctl.h>
#include <pthread.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>
#include <stdarg.h>
#include "libretro.h"
#ifdef HAVE_EGL
#include "glhost.h"
#endif

#define FLAG_SECCOMP 1
#define FLAG_LANDLOCK 2
#define FLAG_LL_NET 4
#define FLAG_NETNS 8
#define FLAG_GPU 16 /* an EGL/GL context was created before the sandbox (HW_RENDER core) */
#define FLAG_GPU_FDS 256 /* the GPU descriptors the context does not need were closed (NVIDIA: everything but /dev/nvidiactl) */
#define FLAG_LL_SCOPE 64 /* Landlock ABI >= 6: signals and abstract unix sockets scoped to the sandbox */
#define FLAG_PIDNS 512 /* the emulator is init (pid 1) of its own PID namespace, supervised by the process the parent started */
#define FLAG_IPC 128 /* own SysV IPC / POSIX mqueue namespace (CLONE_NEWIPC): the host's shm segments of the same uid are not reachable */
#define FLAG_SHM 32 /* private tmpfs on /dev/shm in the child's own mount namespace (PPSSPP builds its emulated RAM as a shm file) */

#ifndef SYS_pidfd_send_signal
#define SYS_pidfd_send_signal 424
#endif
#ifndef SYS_clone3
#define SYS_clone3 435
#endif
/* Landlock: declared locally (not from <linux/landlock.h>) so it builds against old kernel headers (Ubuntu 22.04: 5.15, no net/scope).
 * The ABI actually used is detected at run time (ll_abi()); the kernel ignores nothing: sizes passed to the syscall match the ABI. */
#ifndef SYS_landlock_create_ruleset
#define SYS_landlock_create_ruleset 444
#define SYS_landlock_add_rule 445
#define SYS_landlock_restrict_self 446
#endif
struct totv_ll_ruleset_attr { unsigned long long handled_access_fs, handled_access_net, scoped; };
struct totv_ll_path_beneath_attr { unsigned long long allowed_access; int parent_fd; } __attribute__((packed));
#define LANDLOCK_CREATE_RULESET_VERSION (1U << 0)
#define LANDLOCK_RULE_PATH_BENEATH 1
#define LANDLOCK_ACCESS_FS_EXECUTE (1ULL << 0)
#define LANDLOCK_ACCESS_FS_WRITE_FILE (1ULL << 1)
#define LANDLOCK_ACCESS_FS_READ_FILE (1ULL << 2)
#define LANDLOCK_ACCESS_FS_READ_DIR (1ULL << 3)
#define LANDLOCK_ACCESS_FS_REMOVE_DIR (1ULL << 4)
#define LANDLOCK_ACCESS_FS_REMOVE_FILE (1ULL << 5)
#define LANDLOCK_ACCESS_FS_MAKE_CHAR (1ULL << 6)
#define LANDLOCK_ACCESS_FS_MAKE_DIR (1ULL << 7)
#define LANDLOCK_ACCESS_FS_MAKE_REG (1ULL << 8)
#define LANDLOCK_ACCESS_FS_MAKE_SOCK (1ULL << 9)
#define LANDLOCK_ACCESS_FS_MAKE_FIFO (1ULL << 10)
#define LANDLOCK_ACCESS_FS_MAKE_BLOCK (1ULL << 11)
#define LANDLOCK_ACCESS_FS_MAKE_SYM (1ULL << 12)
#define LANDLOCK_ACCESS_FS_REFER (1ULL << 13)
#define LANDLOCK_ACCESS_FS_TRUNCATE (1ULL << 14)
#define LANDLOCK_ACCESS_FS_IOCTL_DEV (1ULL << 15)
#define LANDLOCK_ACCESS_NET_BIND_TCP (1ULL << 0)
#define LANDLOCK_ACCESS_NET_CONNECT_TCP (1ULL << 1)
#define LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET (1ULL << 0)
#define LANDLOCK_SCOPE_SIGNAL (1ULL << 1)
#ifndef CLONE_NEWTIME
#define CLONE_NEWTIME 0x00000080
#endif
#ifndef IOPRIO_WHO_PROCESS
#define IOPRIO_WHO_PROCESS 1
#endif
#ifndef SYS_io_uring_setup
#define SYS_io_uring_setup 425
#define SYS_io_uring_enter 426
#define SYS_io_uring_register 427
#endif

/* ---------------- sandbox ---------------- */
static int no_scope;  /* option noscope=1 (TESTS ONLY): skip the Landlock signal scope, to prove that seccomp alone stops the F_SETOWN attack */
static int no_pidns;  /* --selftest nopidns / host option pidns=0: skip CLONE_NEWPID */
static int pid_ok;    /* set by sandbox_netns(): unshare(CLONE_NEWPID) worked, the NEXT fork is pid 1 of a new PID namespace (see enter_pidns) */
static int no_ipc;    /* --selftest noipc: pretend CLONE_NEWIPC is not allowed (an old Docker seccomp profile), to exercise the fallback */
static int ipc_ok;    /* set by sandbox_netns(): the child is in its own IPC namespace */
static int deny_fork = 1; /* seccomp denies fork/vfork/clone without CLONE_THREAD (cores only use threads); --selftest turns it off for its own forks */
static int proto_fd_early = -1; /* set in main(); see close_sockets() */
#if defined(__x86_64__)
#define ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define ARCH AUDIT_ARCH_AARCH64
#else
#error unsupported arch
#endif

/* Deny-list (not an allow-list): see "Isolation" in the README for why. Everything here returns EPERM. */
static int sandbox_seccomp(void) {
    static const int blocked[] = {
        /* network (socket() is what keeps UDP out too: Landlock net covers TCP only) */
        SYS_socket, SYS_socketpair, SYS_connect, SYS_bind, SYS_listen, SYS_accept, SYS_accept4, SYS_sendto, SYS_recvfrom,
        SYS_sendmsg, SYS_recvmsg, SYS_sendmmsg, SYS_recvmmsg,
        /* other processes */
        SYS_ptrace, SYS_process_vm_readv, SYS_process_vm_writev, SYS_tkill, SYS_pidfd_send_signal,
        /* io_uring bypasses per-syscall filtering */
        SYS_io_uring_setup, SYS_io_uring_enter, SYS_io_uring_register,
        /* kernel attack surface a libretro core never needs */
        SYS_bpf, SYS_perf_event_open, SYS_userfaultfd, SYS_mount, SYS_umount2, SYS_setns, SYS_keyctl, SYS_add_key, SYS_request_key,
        SYS_kexec_load, SYS_init_module, SYS_finit_module, SYS_delete_module, SYS_pivot_root, SYS_chroot,
        /* a core never creates namespaces (the host did it before the sandbox): unshare() is how it would gain caps in a new userns */
        SYS_unshare,
    };
    /* more EPERM, built at run time */
    int extra[32], nextra = 0;
#ifdef SYS_fork
    if (deny_fork) extra[nextra++] = SYS_fork;
#endif
#ifdef SYS_vfork
    if (deny_fork) extra[nextra++] = SYS_vfork;
#endif
    if (!ipc_ok) { /* no IPC namespace (CLONE_NEWIPC refused by the container's seccomp): at least no SysV IPC / POSIX queues at all */
        extra[nextra++] = SYS_shmget; extra[nextra++] = SYS_shmat; extra[nextra++] = SYS_shmctl; extra[nextra++] = SYS_shmdt;
        extra[nextra++] = SYS_msgget; extra[nextra++] = SYS_msgsnd; extra[nextra++] = SYS_msgrcv; extra[nextra++] = SYS_msgctl;
        extra[nextra++] = SYS_semget; extra[nextra++] = SYS_semop; extra[nextra++] = SYS_semtimedop; extra[nextra++] = SYS_semctl;
        extra[nextra++] = SYS_mq_open;
    }
    /* commands of fcntl()/ioctl() that make a file send a signal to ANOTHER process when it becomes readable/writable (O_ASYNC):
     * F_SETOWN(8) F_SETSIG(10) F_SETOWN_EX(15), FIOSETOWN(0x8901) SIOCSPGRP(0x8902). Without Landlock's signal scope (ABI < 6, kernel < 6.12) a core could
     * point the owner of its stdin pipe at the parent, F_SETSIG it to SIGKILL and be sent the kill by the parent's next input write. */
    static const struct { int nr; int n; unsigned cmd[3]; } deny_cmd[] = {
        { SYS_fcntl, 3, { 8, 10, 15 } },
        { SYS_ioctl, 2, { 0x8901, 0x8902, 0 } },
    };
    enum { MD = sizeof(deny_cmd) / sizeof(deny_cmd[0]) };
    /* everything that acts on ANOTHER process of the same uid (signals, rlimits, scheduling, priorities) is limited to the process itself.
     * The pid is the one at install time (the sandbox is applied by the main thread, whose tid is the pid). A thread id other than the pid is
     * refused too: the core's helper threads then keep their default scheduling, which no core depends on. */
    /* arg0 == own pid (a 0 would mean "my process group" for kill, so it is NOT accepted there) */
    static const int self_pid[] = { SYS_kill, SYS_tgkill, SYS_rt_tgsigqueueinfo, SYS_rt_sigqueueinfo };
    /* arg0 == 0 (the caller) or the own pid */
    static const int self_zero[] = { SYS_prlimit64, SYS_sched_setaffinity, SYS_sched_setscheduler, SYS_sched_setattr, SYS_sched_setparam };
    /* arg0 == PRIO_PROCESS (0) / IOPRIO_WHO_PROCESS (1) and arg1 == 0 or the own pid: never a process group nor a whole user */
    static const struct { int nr; unsigned which; } prio[] = { { SYS_setpriority, 0 }, { SYS_ioprio_set, IOPRIO_WHO_PROCESS } };
    enum { N = sizeof(blocked) / sizeof(blocked[0]), M1 = sizeof(self_pid) / sizeof(self_pid[0]), M2 = sizeof(self_zero) / sizeof(self_zero[0]),
           M3 = sizeof(prio) / sizeof(prio[0]) };
    struct sock_filter f[24 + 2 * (N + 32) + 6 * M1 + 7 * M2 + 9 * M3 + 8 * MD];
    int n = 0;
    unsigned pid = (unsigned)getpid();
    f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, arch));
    f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, ARCH, 1, 0);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr));
#if defined(__x86_64__)
    /* x32 ABI: same AUDIT_ARCH_X86_64, syscall numbers with bit 30 set, and its own table (a classic filter bypass) */
    f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, 0x40000000u, 0, 1);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS);
#endif
    for (int i = 0; i < N; i++) {
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, blocked[i], 0, 1);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
    }
    for (int i = 0; i < nextra; i++) {
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, extra[i], 0, 1);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
    }
    for (int i = 0; i < MD; i++) { /* fcntl/ioctl with one of the owner commands (low 32 bits of arg1: the kernel's "int cmd") */
        int k = deny_cmd[i].n;
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, deny_cmd[i].nr, 0, (unsigned char)(k + 3));
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[1]));
        for (int j = 0; j < k; j++) f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, deny_cmd[i].cmd[j], (unsigned char)(k - j), 0);
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JA, 1, 0, 0);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr));
    }
    /* clone(): any CLONE_NEW* flag is EPERM; without CLONE_THREAD (a new PROCESS, not a thread) it is EPERM too when deny_fork (no cgroup pids.max
     * is guaranteed, and the cores only create threads; RLIMIT_NPROC cannot do this: it counts every process of the uid, see the README) */
    f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_clone, 0, deny_fork ? 4 : 3);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[0]));
    f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, CLONE_NEWNS | CLONE_NEWCGROUP | CLONE_NEWUTS | CLONE_NEWIPC | CLONE_NEWUSER | CLONE_NEWPID | CLONE_NEWNET | CLONE_NEWTIME, deny_fork ? 1 : 0, deny_fork ? 0 : 1);
    if (deny_fork) f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, CLONE_THREAD, 1, 0);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr));
    /* clone3(): its flags live in a struct the filter cannot read. ENOSYS makes glibc (>= 2.34) fall back to clone(), which is filtered above */
    f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_clone3, 0, 1);
    f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | ENOSYS);
    for (int i = 0; i < M1; i++) {
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, self_pid[i], 0, 4);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[0]));
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, pid, 0, 1);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr));
    }
    for (int i = 0; i < M2; i++) {
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, self_zero[i], 0, 5);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[0]));
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, 0, 1, 0);
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, pid, 0, 1);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr));
    }
    for (int i = 0; i < M3; i++) {
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, prio[i].nr, 0, 7);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[0]));
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, prio[i].which, 0, 4);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[1]));
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, 0, 1, 0);
        f[n++] = (struct sock_filter)BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, pid, 0, 1);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM);
        f[n++] = (struct sock_filter)BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr));
    }
    f[n++] = (struct sock_filter)BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW);
    struct sock_fprog prog = { .len = (unsigned short)n, .filter = f };
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)) return 0;
    if (syscall(SYS_seccomp, SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC, &prog)) return 0;
    return 1;
}

/* New user + network namespace (empty: not even loopback is up), then every capability is dropped. Must run while
 * the process is still single-threaded, i.e. before the core is loaded. Best effort: returns FLAG_NETNS or 0
 * (kernel/container without unprivileged user namespaces; the parent then reports netns=false). */
static int want_shm, shm_ok, shm_mb = 512; /* option shmmb=N: size of the private /dev/shm (PSP 512, PCSX2 1024) */ /* option shm=1: private /dev/shm (see the README, "PSP": why the core needs it) */
static int sandbox_netns(void) {
    char before[64] = "", after[64] = "";
    ssize_t b = readlink("/proc/self/ns/net", before, sizeof(before) - 1);
    uid_t u = geteuid(); gid_t g = getegid(); /* before unshare: afterwards they read as the overflow id until mapped */
    /* CLONE_NEWIPC: SysV shm/msg/sem and POSIX queues of the host's IPC namespace (same uid!) are not reachable. A container seccomp profile that
     * predates this flag refuses it: fall back without it (ipc_ok stays 0, and the seccomp filter then denies SysV IPC outright). */
    int base = CLONE_NEWUSER | CLONE_NEWNET | (want_shm ? CLONE_NEWNS : 0);
    /* CLONE_NEWPID too (needs the caps of the new user namespace, so it is requested here): only the children are in it, see enter_pidns(). Same fallback
     * logic as IPC: a profile or kernel that refuses a flag gets the sandbox without it, and the parent sees the missing flag bit. */
    int tries[4] = { CLONE_NEWIPC | CLONE_NEWPID, CLONE_NEWIPC, CLONE_NEWPID, 0 };
    int done = 0;
    for (int i = 0; i < 4 && !done; i++) {
        if ((tries[i] & CLONE_NEWIPC) && no_ipc) continue;
        if ((tries[i] & CLONE_NEWPID) && no_pidns) continue;
        if (!unshare(base | tries[i])) { done = 1; ipc_ok = !!(tries[i] & CLONE_NEWIPC); pid_ok = !!(tries[i] & CLONE_NEWPID); }
    }
    if (!done) return 0;
    if (want_shm) { /* files in a tmpfs owned by our user namespace need a mapped fsuid/fsgid (EOVERFLOW otherwise): map our own ids to themselves */
        char m[64];
        int fd = open("/proc/self/setgroups", O_WRONLY); if (fd >= 0) { if (write(fd, "deny", 4) < 0) {} close(fd); }
        fd = open("/proc/self/uid_map", O_WRONLY); if (fd >= 0) { int n = snprintf(m, sizeof m, "%u %u 1", u, u); if (write(fd, m, (size_t)n) < 0) {} close(fd); }
        fd = open("/proc/self/gid_map", O_WRONLY); if (fd >= 0) { int n = snprintf(m, sizeof m, "%u %u 1", g, g); if (write(fd, m, (size_t)n) < 0) {} close(fd); }
    }
    char shm_opts[48]; snprintf(shm_opts, sizeof shm_opts, "size=%dm,mode=1777", shm_mb);
    if (want_shm) { /* our own mount namespace: nothing below propagates out; the tmpfs hides the host's /dev/shm from the core */
        if (!mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL) && !mount("totv-shm", "/dev/shm", "tmpfs", MS_NOSUID | MS_NODEV | MS_NOEXEC, shm_opts)) shm_ok = 1;
    }
    struct __user_cap_header_struct h = { .version = _LINUX_CAPABILITY_VERSION_3, .pid = 0 };
    struct __user_cap_data_struct d[2]; memset(d, 0, sizeof(d));
    if (syscall(SYS_capset, &h, d)) _exit(3); /* inside the new userns with full caps: refuse to go on */
    ssize_t a = readlink("/proc/self/ns/net", after, sizeof(after) - 1);
    if (a <= 0 || b <= 0 || (a == b && !memcmp(before, after, (size_t)a))) return 0;
    return FLAG_NETNS | (shm_ok ? FLAG_SHM : 0) | (ipc_ok ? FLAG_IPC : 0);
}

/* PID namespace (GPU isolation, option 2). sandbox_netns() asked for CLONE_NEWPID; the process that called unshare() stays in the old namespace and only its
 * children are in the new one, so one fork is needed: the CHILD is pid 1 of the namespace and goes on as the emulator (everything below runs in it), and the
 * process the component started stays outside as a small supervisor that does nothing but wait for it and mirror its exit status. What the namespace gives:
 * the emulator and every thread/process it creates cannot name any process outside (kill/ptrace/pidfd by number never reaches a pid that is not in the
 * namespace, on top of seccomp and Landlock scoping), and when the emulator (init) dies the kernel kills everything left in the namespace. The emulator
 * dies with the supervisor (PR_SET_PDEATHSIG) and the supervisor dies with the component (the component's own Pdeathsig), so the chain is
 * component -> supervisor -> emulator. A lifeline pipe closes the race "supervisor killed before the emulator set Pdeathsig". /proc is NOT remounted
 * (Landlock already denies it; the host's /proc keeps working for the parent's checks). Returns FLAG_PIDNS in the emulator, 0 when it could not be done
 * (the process then goes on in the old namespace); never returns in the supervisor. */
static int enter_pidns(void) {
    if (!pid_ok) return 0;
    int lf[2]; if (pipe2(lf, O_CLOEXEC)) return 0;
    pid_t c = fork();
    if (c < 0) { close(lf[0]); close(lf[1]); return 0; }
    if (c == 0) {
        close(lf[1]);
        prctl(PR_SET_PDEATHSIG, SIGKILL);
        struct pollfd p = { .fd = lf[0], .events = POLLIN };
        if (poll(&p, 1, 0) > 0) _exit(1); /* the supervisor is already gone (nothing is ever written to the lifeline: readable means EOF) */
        close(lf[0]);
        return FLAG_PIDNS;
    }
    /* supervisor: keep only 0,1,2 and the lifeline's write end, then wait */
    DIR *d = opendir("/proc/self/fd");
    if (d) { int dfd = dirfd(d); struct dirent *e;
        while ((e = readdir(d))) { char *end; long fd = strtol(e->d_name, &end, 10); if (!*end && e->d_name[0] != '.' && fd > 2 && fd != dfd && fd != lf[1]) close((int)fd); }
        closedir(d); }
    prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
    sandbox_seccomp(); /* best effort: the supervisor is as constrained as the emulator */
    for (;;) {
        int st; pid_t w = waitpid(c, &st, 0);
        if (w < 0) { if (errno == EINTR) continue; _exit(1); }
        if (WIFEXITED(st)) _exit(WEXITSTATUS(st));
        if (WIFSIGNALED(st)) { signal(WTERMSIG(st), SIG_DFL); raise(WTERMSIG(st)); _exit(128 + WTERMSIG(st)); }
    }
}

static int ll_abi(void) { return (int)syscall(SYS_landlock_create_ruleset, NULL, 0, LANDLOCK_CREATE_RULESET_VERSION); }

/* read-only access to `ro` directories, nothing else on disk. Returns bitmask FLAG_LANDLOCK|FLAG_LL_NET. */
/* GPU device nodes the core may still open after the sandbox (the driver opens /dev/nvidiactl lazily, deep inside its
 * eglcore, on first use of some GL feature): read+write+ioctl on exactly those files, nothing else under /dev. */
static const char *gpu_dev[8]; static int ngpu_dev;
static const char *rw_dir[4]; static int nrw_dir; /* directories the core may read AND write (its save dir), regular files and directories only */
static int sandbox_landlock(const char **ro, int nro) {
    int abi = ll_abi();
    if (abi < 1) return 0;
    unsigned long long fs = (1ULL << 13) - 1; /* ABI1: EXECUTE .. MAKE_SYM */
    if (abi >= 2) fs |= LANDLOCK_ACCESS_FS_REFER;
    if (abi >= 3) fs |= LANDLOCK_ACCESS_FS_TRUNCATE;
    if (abi >= 5) fs |= LANDLOCK_ACCESS_FS_IOCTL_DEV;
    struct totv_ll_ruleset_attr attr = { .handled_access_fs = fs };
    int flags = FLAG_LANDLOCK;
    if (abi >= 4) { attr.handled_access_net = LANDLOCK_ACCESS_NET_BIND_TCP | LANDLOCK_ACCESS_NET_CONNECT_TCP; flags |= FLAG_LL_NET; }
    /* ABI >= 6: the domain may not signal processes outside it (the parent, other sessions) nor connect to abstract unix sockets outside it.
     * Belt and braces on top of the seccomp self_only list (which also covers signals sent with rt_sigqueueinfo etc.). */
    size_t sz = offsetof(struct totv_ll_ruleset_attr, handled_access_net);
    if (abi >= 4) sz = offsetof(struct totv_ll_ruleset_attr, scoped);
    if (abi >= 6 && !no_scope) { attr.scoped = LANDLOCK_SCOPE_SIGNAL | LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET; sz = offsetof(struct totv_ll_ruleset_attr, scoped) + sizeof(attr.scoped); flags |= FLAG_LL_SCOPE; }
    int rs = (int)syscall(SYS_landlock_create_ruleset, &attr, sz, 0);
    if (rs < 0) return 0;
    for (int i = 0; i < nro; i++) {
        int fd = open(ro[i], O_PATH | O_CLOEXEC | O_DIRECTORY);
        if (fd < 0) continue;
        struct totv_ll_path_beneath_attr pb = { .allowed_access = LANDLOCK_ACCESS_FS_READ_FILE | LANDLOCK_ACCESS_FS_READ_DIR, .parent_fd = fd };
        syscall(SYS_landlock_add_rule, rs, LANDLOCK_RULE_PATH_BENEATH, &pb, 0);
        close(fd);
    }
    for (int i = 0; i < nrw_dir; i++) { /* no MAKE_SYM/SOCK/CHAR/BLOCK/FIFO, no EXECUTE: only files and directories under this one */
        int fd = open(rw_dir[i], O_PATH | O_CLOEXEC | O_DIRECTORY);
        if (fd < 0) continue;
        unsigned long long a = LANDLOCK_ACCESS_FS_READ_FILE | LANDLOCK_ACCESS_FS_WRITE_FILE | LANDLOCK_ACCESS_FS_READ_DIR | LANDLOCK_ACCESS_FS_REMOVE_DIR |
                               LANDLOCK_ACCESS_FS_REMOVE_FILE | LANDLOCK_ACCESS_FS_MAKE_DIR | LANDLOCK_ACCESS_FS_MAKE_REG | (abi >= 3 ? LANDLOCK_ACCESS_FS_TRUNCATE : 0) |
                               (abi >= 2 ? LANDLOCK_ACCESS_FS_REFER : 0);
        struct totv_ll_path_beneath_attr pb = { .allowed_access = a, .parent_fd = fd };
        syscall(SYS_landlock_add_rule, rs, LANDLOCK_RULE_PATH_BENEATH, &pb, 0);
        close(fd);
    }
    for (int i = 0; i < ngpu_dev; i++) {
        int fd = open(gpu_dev[i], O_PATH | O_CLOEXEC);
        if (fd < 0) continue;
        unsigned long long a = LANDLOCK_ACCESS_FS_READ_FILE | LANDLOCK_ACCESS_FS_WRITE_FILE | (abi >= 5 ? LANDLOCK_ACCESS_FS_IOCTL_DEV : 0);
        struct totv_ll_path_beneath_attr pb = { .allowed_access = a, .parent_fd = fd };
        syscall(SYS_landlock_add_rule, rs, LANDLOCK_RULE_PATH_BENEATH, &pb, 0);
        close(fd);
    }
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) || syscall(SYS_landlock_restrict_self, rs, 0)) { close(rs); return 0; }
    close(rs);
    return flags;
}

/* Closes every socket descriptor that still exists (never the protocol pipe, stdin or stderr: those are pipes/files). The NVIDIA EGL driver
 * connects to the system D-Bus during its initialisation when the process has a mapped uid (the PSP session's private /dev/shm needs one) and
 * keeps the connection open: seccomp denies sendmsg/recvmsg but a plain write()/read() on that descriptor would still talk to the bus. Nothing
 * the core needs uses it (D-Bus is only the driver's power-profile query), and the sandbox forbids new sockets anyway. */
static int close_sockets(void) {
    int n = 0;
    DIR *d = opendir("/proc/self/fd"); /* only the descriptors that exist: no blind loop over 1024 numbers (a higher fd was missed) */
    if (!d) return 0;
    int dfd = dirfd(d);
    struct dirent *e;
    while ((e = readdir(d))) {
        char *end; long fd = strtol(e->d_name, &end, 10);
        struct stat st;
        if (*end || e->d_name[0] == '.' || fd < 3 || fd == dfd || fd == proto_fd_early || fstat((int)fd, &st) || !S_ISSOCK(st.st_mode)) continue;
        close((int)fd); n++;
    }
    closedir(d);
    return n;
}

/* Threads: of /proc/self/status. Landlock and the seccomp TSYNC would still cover a helper thread started by a driver, Landlock would not
 * (restrict_self is per thread): the host refuses to go on unless the process is single-threaded when the sandbox is applied. */
static int thread_count(void) {
    FILE *f = fopen("/proc/self/status", "r");
    if (!f) return -1;
    char l[256]; int n = -1;
    while (fgets(l, sizeof l, f)) if (!strncmp(l, "Threads:", 8)) { n = atoi(l + 8); break; }
    fclose(f);
    return n;
}

/* A library constructor runs at dlopen(), before the filter: it could have armed a descriptor to signal another process (F_SETOWN + F_SETSIG + O_ASYNC on
 * the stdin pipe: the parent's next write would then deliver the signal). Undo that on every descriptor right before the sandbox; afterwards seccomp
 * refuses the commands that would set it again. */
static int scrub_async(void) {
    int n = 0;
    DIR *d = opendir("/proc/self/fd");
    if (!d) return -1;
    int dfd = dirfd(d);
    struct dirent *e;
    while ((e = readdir(d))) {
        char *end; long fd = strtol(e->d_name, &end, 10);
        if (*end || e->d_name[0] == '.' || fd == dfd) continue;
        int fl = fcntl((int)fd, F_GETFL);
        if (fl < 0) continue;
        if (fl & O_ASYNC) { fcntl((int)fd, F_SETFL, fl & ~O_ASYNC); n++; }
        if (fcntl((int)fd, F_GETOWN) != 0) { fcntl((int)fd, F_SETOWN, 0); n++; }
        fcntl((int)fd, F_SETSIG, 0);
    }
    closedir(d);
    return n;
}

static int sandbox(const char **ro, int nro) {
    scrub_async();
    int fl = sandbox_landlock(ro, nro);
    if (sandbox_seccomp()) fl |= FLAG_SECCOMP;
    return fl;
}


/* ---- experiment hook (debug only, needs RETRO_EMU_LOG): RETRO_EMU_FDX="substr[=keep],..." closes the fds whose target contains substr, keeping the first <keep> */
static void fd_report(const char *tag) {
    DIR *d = opendir("/proc/self/fd"); if (!d) return;
    int dfd = dirfd(d); struct dirent *e; fprintf(stderr, "FDS %s:", tag);
    while ((e = readdir(d))) { char *end; long fd = strtol(e->d_name, &end, 10); if (*end || e->d_name[0] == '.' || fd == dfd) continue;
        char l[256], p[64]; snprintf(p, sizeof p, "/proc/self/fd/%ld", fd); ssize_t n = readlink(p, l, sizeof l - 1); if (n < 0) n = 0; l[n] = 0; fprintf(stderr, " %ld=%s", fd, l); }
    fprintf(stderr, "\n"); closedir(d);
}
#ifdef DEBUG
static void fd_experiment(void) {
    const char *x = getenv("RETRO_EMU_FDX"); if (!x) return;
    char *spec = strdup(x);
    for (char *tok = strtok(spec, ","); tok; tok = strtok(NULL, ",")) {
        if (tok[0] == '#') { close(atoi(tok + 1)); continue; }
        int keep = 0; char *eq = strchr(tok, '='); if (eq) { *eq = 0; keep = atoi(eq + 1); }
        DIR *d = opendir("/proc/self/fd"); if (!d) continue; int dfd = dirfd(d); struct dirent *e; int seen = 0;
        while ((e = readdir(d))) { char *end; long fd = strtol(e->d_name, &end, 10); if (*end || e->d_name[0] == '.' || fd < 3 || fd == dfd || fd == proto_fd_early) continue;
            char l[256], p[64]; snprintf(p, sizeof p, "/proc/self/fd/%ld", fd); ssize_t n = readlink(p, l, sizeof l - 1); if (n < 0) continue; l[n] = 0;
            if (strstr(l, tok) && seen++ >= keep) close((int)fd); }
        closedir(d);
    }
    free(spec);
}
#endif /* DEBUG */

/* ---- minimal GPU descriptor surface (GPU isolation, option 2) ----
 * Measured (README, "GPU isolation (opção 2)"): with the NVIDIA driver (open kernel module 620.x) EGL(platform device) + GLideN64 + PPSSPP GL + the glcore
 * test core render for 60 s with ONLY /dev/nvidiactl open. Everything else the driver opened during the context creation is closed right before the
 * sandbox: /dev/nvidia0 (x12; its fds are only needed to create/map memory, the context already exists), /dev/nvidia-modeset (NVKMS: display modesetting,
 * the most dangerous node for the desktop), /dev/nvidia-uvm, nvidia-uvm-tools and nvidia-caps (never opened by GL, closed anyway), everything under /dev/dri (the render node
 * is only used by the Mesa path). /dev/nvidiactl stays: libnvidia-eglcore uses it lazily (Landlock keeps one rule for it, opened AFTER the sandbox,
 * because the rule IOCTL_DEV only applies to descriptors opened after the restriction). A driver that is not NVIDIA's (Mesa) keeps its render node open and
 * is not touched. Option gfd=keep disables this (tests/diagnosis). */
static int gpu_is_nvidia(void) {
#ifdef HAVE_EGL
    return strstr(G.version, "NVIDIA") != NULL;
#else
    return 0;
#endif
}
static int gpu_fd_keep = 0;   /* option gfd=keep */
static int gpu_fds_closed;    /* how many descriptors close_gpu_fds() closed */
static int close_gpu_fds(void) {
    if (gpu_fd_keep || !gpu_is_nvidia()) return 0;
    int n = 0;
    /* The numbers are RESERVED, not freed: each GPU descriptor is dup3()'d over by /dev/null (O_CLOEXEC). A plain close() would let the next open() (ISO, save,
     * shm) get the same number, and a driver that still uses its old number would then hit an unrelated file; on /dev/null it only gets ENOTTY/ENODEV. */
    int nullfd = open("/dev/null", O_RDWR | O_CLOEXEC); if (nullfd < 0) return -1;
    DIR *d = opendir("/proc/self/fd"); if (!d) { close(nullfd); return -1; }
    int dfd = dirfd(d); struct dirent *e;
    while ((e = readdir(d))) {
        char *end; long fd = strtol(e->d_name, &end, 10);
        if (*end || e->d_name[0] == '.' || fd < 3 || fd == dfd || fd == nullfd || fd == proto_fd_early) continue;
        char l[256], p[64]; snprintf(p, sizeof p, "/proc/self/fd/%ld", fd);
        ssize_t k = readlink(p, l, sizeof l - 1); if (k < 0) continue; l[k] = 0;
        if (!strncmp(l, "/dev/nvidia", 11) && strcmp(l, "/dev/nvidiactl")) { if (dup3(nullfd, (int)fd, O_CLOEXEC) < 0) { closedir(d); close(nullfd); return -1; } n++; }
        else if (!strncmp(l, "/dev/dri/", 9)) { if (dup3(nullfd, (int)fd, O_CLOEXEC) < 0) { closedir(d); close(nullfd); return -1; } n++; }
    }
    closedir(d);
    close(nullfd);
    gpu_fds_closed = n;
    return n;
}

/* the PID namespace, exercised through the real code (sandbox_netns + enter_pidns) in a forked process: the emulator side must be pid 1 and report FLAG_PIDNS,
 * the supervisor side mirrors its exit status (so 0 here means both sides worked). */
static int selftest_pidns(void) {
    fflush(stdout);
    pid_t c = fork(); if (c < 0) return 0;
    if (c == 0) {
        no_pidns = 0; deny_fork = 0;
        int fl = sandbox_netns(); if (!(fl & FLAG_NETNS) || !pid_ok) _exit(10);
        int r = enter_pidns(); /* only the emulator returns; the supervisor exits with its status */
        _exit(r == FLAG_PIDNS && getpid() == 1 && getppid() == 0 ? 0 : 11);
    }
    int st = 0; return waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0;
}

/* proves the sandbox from the inside: used at component start (health) and by the tests */
#define BLOCKED(call, err) ((call) == -1 && errno == (err))
static int selftest(void) {
    const char *ro[] = { "/usr/lib", "/lib", "/usr/lib64", "/lib64" };
    int before_open = open("/proc/self/stat", O_RDONLY); if (before_open >= 0) close(before_open);
    /* a SysV shm segment of the host's IPC namespace (same uid), attached and marked for removal (it vanishes when this process ends):
     * after CLONE_NEWIPC the sandbox must not be able to attach it again */
    int shmid = shmget(IPC_PRIVATE, 4096, IPC_CREAT | 0600); void *shm_pre = shmid >= 0 ? shmat(shmid, NULL, 0) : (void *)-1;
    if (shmid >= 0) shmctl(shmid, IPC_RMID, NULL);
    int shm_pre_ok = shm_pre != (void *)-1;
    int pidns_ok = selftest_pidns();
    no_pidns = 1; /* the rest of the selftest forks several times: a PID namespace dies with its first child */
    deny_fork = 0; /* this selftest forks for its own checks; the fork denial is proved below in a child that adds its own filter */
    int fl = sandbox_netns();
    /* Landlock's signal scope on its own (no seccomp yet): a forked child applies only the ruleset and must not reach the parent. ABI < 6: n/a */
    int scope_ok = 0;
    if (ll_abi() >= 6) {
        pid_t c = fork();
        if (c == 0) { sandbox_landlock(ro, 4); errno = 0; _exit(kill(getppid(), 0) == -1 && errno == EPERM ? 0 : 1); }
        int st = 0; scope_ok = c > 0 && waitpid(c, &st, 0) == c && WIFEXITED(st) && WEXITSTATUS(st) == 0;
    }
    fl |= sandbox(ro, 4);
    errno = 0; int s = socket(AF_INET, SOCK_DGRAM, 0); int e_sock = errno; if (s >= 0) close(s);
    errno = 0; int s2 = socket(AF_INET, SOCK_STREAM, 0); int e_tcp = errno; if (s2 >= 0) close(s2);
    errno = 0; int a = open("/proc/self/stat", O_RDONLY); int e_open = errno; if (a >= 0) close(a);
    errno = 0; int pw = open("/etc/passwd", O_RDONLY); int e_pw = errno; if (pw >= 0) close(pw);
    errno = 0; long pt = syscall(SYS_ptrace, 0, 0, 0, 0); int e_pt = errno;
    errno = 0; long ur = syscall(SYS_io_uring_setup, 8, NULL); int e_ur = errno;
    errno = 0; long kl = kill(getppid(), 0); int e_kl = errno; /* the parent: must not be reachable */
    errno = 0; long ks = kill(getpid(), 0);                    /* ourselves: still allowed */
    pid_t pp = getppid();
    /* other processes of the same uid: signals with data, rlimits, scheduling, priorities (the parent is the victim) */
    struct rlimit old;
    errno = 0; int sq = BLOCKED(syscall(SYS_rt_sigqueueinfo, pp, 0, NULL), EPERM);
    errno = 0; int pl = BLOCKED(syscall(SYS_prlimit64, pp, RLIMIT_NOFILE, NULL, &old), EPERM);
    errno = 0; int pl_ok = syscall(SYS_prlimit64, 0, RLIMIT_NOFILE, NULL, &old) == 0; /* itself: allowed */
    errno = 0; int sp = BLOCKED(syscall(SYS_setpriority, 0, pp, 0), EPERM) && BLOCKED(syscall(SYS_setpriority, 1 /* PRIO_PGRP */, 0, 0), EPERM) &&
                        BLOCKED(syscall(SYS_setpriority, 2 /* PRIO_USER */, 0, 0), EPERM);
    errno = 0; int sp_ok = syscall(SYS_setpriority, 0, 0, getpriority(PRIO_PROCESS, 0)) == 0;
    cpu_set_t cs; CPU_ZERO(&cs); sched_getaffinity(0, sizeof cs, &cs);
    errno = 0; int sa = BLOCKED(syscall(SYS_sched_setaffinity, pp, sizeof cs, &cs), EPERM);
    errno = 0; int sa_ok = syscall(SYS_sched_setaffinity, 0, sizeof cs, &cs) == 0;
    errno = 0; int ss = BLOCKED(syscall(SYS_sched_setscheduler, pp, 0, NULL), EPERM) && BLOCKED(syscall(SYS_sched_setattr, pp, NULL, 0), EPERM) &&
                        BLOCKED(syscall(SYS_sched_setparam, pp, NULL), EPERM);
    errno = 0; int io = BLOCKED(syscall(SYS_ioprio_set, 1, pp, 0), EPERM) && BLOCKED(syscall(SYS_ioprio_set, 3 /* IOPRIO_WHO_USER */, 0, 0), EPERM);
    /* namespaces: unshare/clone(CLONE_NEW*) EPERM, clone3 ENOSYS (glibc falls back to clone) */
    errno = 0; int un = BLOCKED(unshare(CLONE_NEWUSER), EPERM) && BLOCKED(unshare(CLONE_NEWNET), EPERM);
    errno = 0; int cl = BLOCKED(syscall(SYS_clone, CLONE_NEWNET | SIGCHLD, 0, 0, 0, 0), EPERM) && BLOCKED(syscall(SYS_clone, CLONE_NEWUSER | SIGCHLD, 0, 0, 0, 0), EPERM);
    errno = 0; int c3 = BLOCKED(syscall(SYS_clone3, NULL, 0), ENOSYS);
    int thr_ok = 1; /* plain fork() (a clone without CLONE_NEW*) still works */
    { pid_t c = fork(); if (c == 0) _exit(0); int st = 0; thr_ok = c > 0 && waitpid(c, &st, 0) == c && WIFEXITED(st); }
    /* SysV IPC of the host: the segment created before the namespace is not reachable (EINVAL in the new ipc ns; EPERM when the fallback denies SysV IPC) */
    errno = 0; void *shm_post = shm_pre_ok ? shmat(shmid, NULL, 0) : (void *)0; int e_shm = errno;
    int ipc_ok_t = shm_pre_ok && shm_post == (void *)-1 && e_shm == (ipc_ok ? EINVAL : EPERM);
    /* async owner: a pipe of our own, the commands that would make it signal another process are refused, the ordinary ones still work */
    int async_ok = 0;
    {
        int pf[2];
        if (pipe(pf) == 0) {
            int one = (int)pp; struct { int type; int pid; } fo = { 1 /* F_OWNER_TID */, (int)pp };
            int b1 = BLOCKED(fcntl(pf[0], F_SETOWN, pp), EPERM), b2 = BLOCKED(fcntl(pf[0], 15 /* F_SETOWN_EX */, &fo), EPERM),
                b3 = BLOCKED(fcntl(pf[0], 10 /* F_SETSIG */, SIGKILL), EPERM), b4 = BLOCKED(ioctl(pf[0], 0x8901 /* FIOSETOWN */, &one), EPERM),
                b5 = BLOCKED(ioctl(pf[0], 0x8902 /* SIOCSPGRP */, &one), EPERM), b6 = BLOCKED(fcntl(pf[0], F_SETOWN, getpid()), EPERM);
            int g = fcntl(pf[0], F_GETFL), ok1 = g >= 0, ok2 = fcntl(pf[0], F_SETFL, g | O_NONBLOCK) == 0, ok3 = (fcntl(pf[0], F_GETFL) & O_NONBLOCK) != 0,
                ok4 = fcntl(pf[0], F_GETFD) >= 0, ok5 = fcntl(pf[0], F_GETOWN) == 0; /* nobody owns it */
            async_ok = b1 && b2 && b3 && b4 && b5 && b6 && ok1 && ok2 && ok3 && ok4 && ok5;
            close(pf[0]); close(pf[1]);
        }
    }
    if (ll_abi() < 6) scope_ok = async_ok; /* no Landlock scope on this kernel: the equivalent protection is the seccomp one */
    /* no new processes: a child adds the fork denial (what the host applies for real) on top of this filter */
    int fork_denied = 0, thr_after = 0;
    {
        pid_t c = fork();
        if (c == 0) {
            deny_fork = 1; if (!sandbox_seccomp()) _exit(100);
            int r = 0; errno = 0;
            { pid_t x = fork(); if (x == 0) _exit(0); if (x == -1 && errno == EPERM) r |= 1; }
            errno = 0; { long x = syscall(SYS_clone, SIGCHLD, 0, 0, 0, 0); if (x == 0) _exit(0); if (x == -1 && errno == EPERM) r |= 2; }
#ifdef SYS_vfork
            errno = 0; { long x = syscall(SYS_vfork); if (x == 0) _exit(0); if (x == -1 && errno == EPERM) r |= 8; }
#else
            r |= 8;
#endif
            { pthread_t t; if (pthread_create(&t, NULL, (void *(*)(void *))getpid, NULL) == 0 && pthread_join(t, NULL) == 0) r |= 4; }
            _exit(r);
        }
        int st = 0;
        if (c > 0 && waitpid(c, &st, 0) == c && WIFEXITED(st)) { fork_denied = (WEXITSTATUS(st) & 11) == 11; thr_after = (WEXITSTATUS(st) & 4) != 0; }
    }
    int udp_ok = s < 0 && e_sock == EPERM, tcp_ok = s2 < 0 && e_tcp == EPERM;
    int x32_ok = 1;
#if defined(__x86_64__)
    { /* an x32 syscall number must kill the process (checked in a forked child, which inherits the filter) */
        pid_t c = fork();
        if (c == 0) { syscall(0x40000000L | SYS_getpid); _exit(0); }
        int st = 0; x32_ok = c > 0 && waitpid(c, &st, 0) == c && WIFSIGNALED(st) && WTERMSIG(st) == SIGSYS;
    }
#endif
    int others_ok = sq && pl && pl_ok && sp && sp_ok && sa && sa_ok && ss && io;
    int ns_ok = un && cl && c3 && thr_ok && ipc_ok_t && async_ok && fork_denied && thr_after;
    int seccomp_ok = (fl & FLAG_SECCOMP) && udp_ok && tcp_ok && pt == -1 && e_pt == EPERM && ur == -1 && e_ur == EPERM &&
                     kl == -1 && e_kl == EPERM && ks == 0 && x32_ok && others_ok && ns_ok;
    int landlock_ok = (fl & FLAG_LANDLOCK) && before_open >= 0 && a < 0 && e_open == EACCES && pw < 0 && e_pw == EACCES && scope_ok;
#define TF(x) ((x) ? "true" : "false")
    printf("{\"seccomp\":%s,\"landlock\":%s,\"landlock_abi\":%d,\"landlock_net\":%s,\"landlock_scope\":%s,\"netns\":%s,\"udp_blocked\":%s,"
           "\"passwd_blocked\":%s,\"io_uring_blocked\":%s,\"kill_other_blocked\":%s,\"x32_killed\":%s,"
           "\"sigqueue_other_blocked\":%s,\"prlimit_other_blocked\":%s,\"priority_other_blocked\":%s,\"sched_other_blocked\":%s,\"ioprio_other_blocked\":%s,"
           "\"self_allowed\":%s,\"unshare_blocked\":%s,\"clone_newns_blocked\":%s,\"clone3_enosys\":%s,\"fork_works\":%s,"
           "\"async_owner_blocked\":%s,\"ipcns\":%s,\"sysv_unreachable\":%s,\"fork_blocked\":%s,\"thread_works\":%s,\"pidns\":%s}\n",
           TF(seccomp_ok), TF(landlock_ok), ll_abi(), TF(fl & FLAG_LL_NET), TF(fl & FLAG_LL_SCOPE && scope_ok), TF(fl & FLAG_NETNS), TF(udp_ok), TF(pw < 0 && e_pw == EACCES),
           TF(ur == -1 && e_ur == EPERM), TF(kl == -1 && e_kl == EPERM), TF(x32_ok), TF(sq), TF(pl), TF(sp), TF(sa && ss), TF(io),
           TF(ks == 0 && pl_ok && sp_ok && sa_ok), TF(un), TF(cl), TF(c3), TF(thr_ok),
           TF(async_ok), TF(fl & FLAG_IPC), TF(ipc_ok_t), TF(fork_denied), TF(thr_after), TF(pidns_ok));
    return 0;
}

/* ---------------- libretro host ---------------- */
static void (*r_set_env)(retro_environment_t), (*r_set_video)(retro_video_refresh_t), (*r_set_asb)(retro_audio_sample_batch_t),
    (*r_set_as)(retro_audio_sample_t), (*r_set_poll)(retro_input_poll_t), (*r_set_input)(retro_input_state_t);
static void (*r_init)(void), (*r_run)(void), (*r_deinit)(void);
static bool (*r_load)(const struct retro_game_info *);
static void (*r_av)(struct retro_system_av_info *), (*r_sysinfo)(struct retro_system_info *);
static void *(*r_mem)(unsigned);
static size_t (*r_memsize)(unsigned);

static unsigned out_w = 640, out_h = 480, pixfmt = RETRO_PIXEL_FORMAT_0RGB1555;
static uint32_t *outbuf; static int have_frame;
static uint8_t *i420;
static int16_t *abuf; static size_t acount, acap;
static unsigned pad[4]; static int8_t axes[4][4]; /* per port: left X, left Y, right X, right Y */
static const char *sysdir = "/nonexistent";
static const char *savedir; /* option save=<dir>: the one directory the core may write (PSP memory stick: PSP/SAVEDATA, shader cache) */
static const char *ro_extra[3]; static int nro_extra; /* option ro=<dir>: more read-only directories (PPSSPP assets) */

static int proto_fd = 1; /* private copy of the pipe to the parent; fd 1 is redirected to stderr for the core */
static int wr_all(const void *p, size_t n) {
    const char *c = p;
    while (n) { ssize_t w = write(proto_fd, c, n); if (w < 0) { if (errno == EINTR) continue; return -1; } c += w; n -= (size_t)w; }
    return 0;
}
static int send_msg(char type, const void *p, uint32_t len) {
    unsigned char h[8] = { (unsigned char)type, 0, 0, 0 };
    memcpy(h + 4, &len, 4);
    if (wr_all(h, 8)) return -1;
    return len ? wr_all(p, len) : 0;
}

/* core options given by the parent (arg 7: "v.KEY=VALUE;..."): GET_VARIABLE answers from this table, unknown keys are "not set" */
#define MAXVARS 96
static struct { char *k, *v; } vars[MAXVARS]; static int nvars;
static struct { char *k, *v; } defs[MAXVARS * 3]; static int ndefs; /* defaults announced by the core with SET_VARIABLES */
static int dbg;
static double ms_since(const struct timespec *a) { struct timespec n; clock_gettime(CLOCK_MONOTONIC, &n); return (double)(n.tv_sec - a->tv_sec) * 1e3 + (n.tv_nsec - a->tv_nsec) / 1e6; }
static void log_cb(int level, const char *fmt, ...) {
    if (!dbg) return;
    va_list ap; va_start(ap, fmt); fprintf(stderr, "[core %d] ", level); vfprintf(stderr, fmt, ap); va_end(ap);
}
static unsigned want_gl_type, want_gl_major, want_gl_minor; /* from the options: what the core must ask for (0 = software core) */
static int hw_mode;     /* a HW_RENDER context was accepted */
static unsigned frame_w, frame_h;

static bool env_cb(unsigned cmd, void *data) {
    switch (cmd) {
    case RETRO_ENVIRONMENT_GET_VARIABLE: {
        struct retro_variable *v = data;
        for (int i = 0; i < nvars; i++) if (!strcmp(vars[i].k, v->key)) { v->value = vars[i].v; return true; }
        for (int i = 0; i < ndefs; i++) if (!strcmp(defs[i].k, v->key)) { v->value = defs[i].v; return true; }
        v->value = NULL; return false;
    }
    case RETRO_ENVIRONMENT_SET_VARIABLES: { /* {key, "Description; default|other|..."}, ended by a NULL key: the first value is the default */
        for (const struct retro_variable *v = data; v->key && ndefs < MAXVARS * 3; v++) {
            const char *sep = v->value ? strstr(v->value, "; ") : NULL; if (!sep) continue;
            char *d = strdup(sep + 2); char *bar = strchr(d, '|'); if (bar) *bar = 0;
            defs[ndefs].k = strdup(v->key); defs[ndefs++].v = d;
        }
        return true;
    }
    case RETRO_ENVIRONMENT_GET_VARIABLE_UPDATE: *(bool *)data = false; return true;
    case RETRO_ENVIRONMENT_GET_LOG_INTERFACE: ((struct retro_log_callback *)data)->log = log_cb; return true;
    case RETRO_ENVIRONMENT_SET_GEOMETRY: case RETRO_ENVIRONMENT_SET_SYSTEM_AV_INFO: return true;
#ifdef HAVE_EGL
    case RETRO_ENVIRONMENT_SET_HW_RENDER:
        if (!want_gl_type || !gl_hw_render(data)) { log_cb(3, "SET_HW_RENDER refused (core asked type %u v%u.%u)\n", ((struct retro_hw_render_callback *)data)->context_type, ((struct retro_hw_render_callback *)data)->version_major, ((struct retro_hw_render_callback *)data)->version_minor); return false; }
        hw_mode = 1; return true;
    case RETRO_ENVIRONMENT_GET_PREFERRED_HW_RENDER: if (!want_gl_type) return false; *(unsigned *)data = want_gl_type; return true;
#endif
    case RETRO_ENVIRONMENT_GET_CAN_DUPE: *(bool *)data = true; return true;
    case RETRO_ENVIRONMENT_SET_PIXEL_FORMAT: pixfmt = *(unsigned *)data; return pixfmt <= 2;
    case RETRO_ENVIRONMENT_GET_SYSTEM_DIRECTORY: *(const char **)data = sysdir; return true;
    case RETRO_ENVIRONMENT_GET_SAVE_DIRECTORY: *(const char **)data = savedir ? savedir : sysdir; return true;
    case RETRO_ENVIRONMENT_SET_SUPPORT_ACHIEVEMENTS: return true;
    default: return false;
    }
}

static double t_blit_ms, t_read_ms;
static void video_cb(const void *data, unsigned w, unsigned h, size_t pitch) {
    if (!data || !w || !h) return; /* duplicate frame: keep the last one */
#ifdef HAVE_EGL
    if (data == RETRO_HW_FRAME_BUFFER_VALID) {
        double tb, tr; frame_w = w; frame_h = h;
        static int nlog; if (dbg && nlog++ < 5) fprintf(stderr, "hw video_cb %ux%u\n", w, h);
        if (gl_frame_end(w, h, out_w, out_h, &tb, &tr)) have_frame = 2; /* 2: outbuf already filled (BGRA) */
        t_blit_ms = tb * 1e3; t_read_ms = tr * 1e3;
        return;
    }
#endif
    static unsigned *xmap; static unsigned xmap_w;
    if (xmap_w != w) { free(xmap); xmap = malloc(out_w * sizeof(unsigned)); for (unsigned x = 0; x < out_w; x++) xmap[x] = x * w / out_w; xmap_w = w; }
    for (unsigned y = 0; y < out_h; y++) {
        const unsigned char *row = (const unsigned char *)data + (size_t)(y * h / out_h) * pitch;
        uint32_t *o = outbuf + (size_t)y * out_w;
        if (pixfmt == RETRO_PIXEL_FORMAT_XRGB8888) { const uint32_t *s = (const uint32_t *)row; for (unsigned x = 0; x < out_w; x++) o[x] = s[xmap[x]]; }
        else if (pixfmt == RETRO_PIXEL_FORMAT_RGB565) {
            const uint16_t *s = (const uint16_t *)row;
            for (unsigned x = 0; x < out_w; x++) { uint16_t p = s[xmap[x]]; uint32_t r = (p >> 11) & 31, g = (p >> 5) & 63, b = p & 31;
                o[x] = ((r << 3 | r >> 2) << 16) | ((g << 2 | g >> 4) << 8) | (b << 3 | b >> 2); }
        } else {
            const uint16_t *s = (const uint16_t *)row;
            for (unsigned x = 0; x < out_w; x++) { uint16_t p = s[xmap[x]]; uint32_t r = (p >> 10) & 31, g = (p >> 5) & 31, b = p & 31;
                o[x] = ((r << 3 | r >> 2) << 16) | ((g << 3 | g >> 2) << 8) | (b << 3 | b >> 2); }
        }
    }
    have_frame = 1;
}
/* BGRA -> I420 (BT.601, limited range); chroma = average of each 2x2 block */
static void to_i420(void) {
    uint8_t *Y = i420, *U = Y + (size_t)out_w * out_h, *V = U + (size_t)(out_w / 2) * (out_h / 2);
    for (unsigned y = 0; y < out_h; y++) {
        const uint32_t *r0 = outbuf + (size_t)y * out_w;
        for (unsigned x = 0; x < out_w; x++) {
            uint32_t p = r0[x]; int r = (p >> 16) & 255, g = (p >> 8) & 255, b = p & 255;
            Y[(size_t)y * out_w + x] = (uint8_t)(((66 * r + 129 * g + 25 * b + 128) >> 8) + 16);
        }
    }
    for (unsigned y = 0; y + 1 < out_h; y += 2) for (unsigned x = 0; x + 1 < out_w; x += 2) {
        const uint32_t *a = outbuf + (size_t)y * out_w + x, *c = a + out_w;
        int r = (((a[0] >> 16) & 255) + ((a[1] >> 16) & 255) + ((c[0] >> 16) & 255) + ((c[1] >> 16) & 255)) >> 2;
        int g = (((a[0] >> 8) & 255) + ((a[1] >> 8) & 255) + ((c[0] >> 8) & 255) + ((c[1] >> 8) & 255)) >> 2;
        int b = ((a[0] & 255) + (a[1] & 255) + (c[0] & 255) + (c[1] & 255)) >> 2;
        size_t i = (size_t)(y / 2) * (out_w / 2) + x / 2;
        U[i] = (uint8_t)(((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128);
        V[i] = (uint8_t)(((112 * r - 94 * g - 18 * b + 128) >> 8) + 128);
    }
}
static size_t audio_batch_cb(const int16_t *d, size_t frames) {
    if ((acount + frames) * 2 > acap) { acap = (acount + frames) * 2 + 4096; abuf = realloc(abuf, acap * sizeof(int16_t)); }
    memcpy(abuf + acount * 2, d, frames * 4); acount += frames; return frames;
}
static void audio_cb(int16_t l, int16_t r) { int16_t t[2] = { l, r }; audio_batch_cb(t, 1); }
static void poll_cb(void) {}
static int16_t input_cb(unsigned port, unsigned dev, unsigned idx, unsigned id) {
    if (port > 3) return 0;
    if (dev == RETRO_DEVICE_ANALOG) { /* int8 -128..127 -> int16 (x256); Y positive = down, as libretro */
        if (idx > 1 || id > 1) return 0;
        return (int16_t)(axes[port][idx * 2 + id] * 256);
    }
    if (dev != RETRO_DEVICE_JOYPAD || idx) return 0;
    if (id == RETRO_DEVICE_ID_JOYPAD_MASK) return (int16_t)pad[port];
    return id < 16 ? (pad[port] >> id) & 1 : 0;
}

static unsigned char inbuf[256]; static size_t inlen; static int want_quit;
/* drains the control pipe; returns -1 on EOF (parent gone) */
static int drain_input(void) {
    struct pollfd p = { .fd = 0, .events = POLLIN };
    while (poll(&p, 1, 0) > 0 && (p.revents & (POLLIN | POLLHUP))) {
        ssize_t r = read(0, inbuf + inlen, sizeof(inbuf) - inlen);
        if (r == 0) return -1;
        if (r < 0) { if (errno == EINTR) continue; return -1; }
        inlen += (size_t)r;
        size_t i = 0;
        for (; i + 4 <= inlen; i += 4) {
            if (inbuf[i] == 1 && inbuf[i + 3] < 4) pad[inbuf[i + 3]] = inbuf[i + 1] | (inbuf[i + 2] << 8);
            else if (inbuf[i] == 3 && inbuf[i + 1] < 4 && inbuf[i + 2] < 4) axes[inbuf[i + 1]][inbuf[i + 2]] = (int8_t)inbuf[i + 3]; /* [3, port, axis, int8] */
            else if (inbuf[i] == 2) want_quit = 1;
        }
        memmove(inbuf, inbuf + i, inlen - i); inlen -= i;
    }
    return 0;
}

static void fail(const char *m) { send_msg('E', m, (uint32_t)strlen(m)); _exit(2); }

static void add_ns(struct timespec *t, long ns) { t->tv_nsec += ns; while (t->tv_nsec >= 1000000000L) { t->tv_nsec -= 1000000000L; t->tv_sec++; } }

int main(int argc, char **argv) {
    if (argc >= 2 && !strcmp(argv[1], "--selftest")) { no_ipc = argc > 2 && !strcmp(argv[2], "noipc"); return selftest(); }
    if (argc == 2 && !strcmp(argv[1], "--gltest")) { /* is there a usable GPU for HW_RENDER cores? (health: "gpu") */
#ifdef HAVE_EGL
        const char *e = gl_pre_init(RETRO_HW_CONTEXT_OPENGL_CORE, 4, 6);
        if (e) { printf("{\"ok\":false,\"error\":\"%s\"}\n", e); return 0; }
        printf("{\"ok\":true,\"renderer\":\"%s\",\"version\":\"%s\"}\n", G.renderer, G.version);
#else
        printf("{\"ok\":false,\"error\":\"built without EGL\"}\n");
#endif
        return 0;
    }
    if (argc != 7 && argc != 8) { fprintf(stderr, "usage: retro-emu core rom sram|- sysdir w h [opts]\n"); return 64; }
    /* The protocol must not share fd 1 with the core: anything the core prints (snes9x writes 'Map_LoROMMap' at exit)
     * would be read by the parent as a frame header. Keep a private copy of the pipe and point fd 1 at stderr. */
    if (argc == 7 || argc == 8) {
        int fd = fcntl(1, F_DUPFD_CLOEXEC, 10);
        if (fd < 0 || dup2(2, 1) < 0) return 1;
        proto_fd = fd; proto_fd_early = fd;
    }
    dbg = getenv("RETRO_EMU_LOG") != NULL; /* debugging only: the parent starts the child with an empty environment */
    /* the driver must not write a shader cache nor start helper threads of its own after the sandbox */
    setenv("__GL_SHADER_DISK_CACHE", "0", 1); setenv("__GL_THREADED_OPTIMIZATIONS", "0", 1);
    int rb_sync = 1, linear = 1, gpu_stats = 1; (void)gpu_stats;
    unsigned long long fsize = 512ULL << 20; /* option fsize=<bytes>: RLIMIT_FSIZE, the largest file the core can create or grow */
    if (argc == 8) { /* options "k=v;k=v": gl=core33|gl33|none, rb=sync (default)|pbo, filter=linear|nearest, v.<core option>=<value> */
        char *o = strdup(argv[7]);
        for (char *tok = strtok(o, ";"); tok; tok = strtok(NULL, ";")) {
            if (!strncmp(tok, "v.", 2) && strchr(tok, '=') && nvars < MAXVARS) { char *eq = strchr(tok, '='); *eq = 0; vars[nvars].k = strdup(tok + 2); vars[nvars++].v = strdup(eq + 1); }
            else if (!strcmp(tok, "gl=core33")) { want_gl_type = RETRO_HW_CONTEXT_OPENGL_CORE; want_gl_major = 3; want_gl_minor = 3; }
            else if (!strcmp(tok, "gl=core46")) { want_gl_type = RETRO_HW_CONTEXT_OPENGL_CORE; want_gl_major = 4; want_gl_minor = 6; } /* context created 4.6; a core asking for <= is accepted */
            else if (!strcmp(tok, "gl=gl33")) { want_gl_type = RETRO_HW_CONTEXT_OPENGL; want_gl_major = 3; want_gl_minor = 3; }
            else if (!strncmp(tok, "dev=/dev/", 9) && ngpu_dev < 8) gpu_dev[ngpu_dev++] = strdup(tok + 4);
            else if (!strncmp(tok, "save=/", 6) && !savedir) { savedir = strdup(tok + 5); rw_dir[nrw_dir++] = savedir; }
            else if (!strncmp(tok, "fsize=", 6)) fsize = strtoull(tok + 6, NULL, 10);
            else if (!strcmp(tok, "shm=1")) { want_shm = 1; rw_dir[nrw_dir++] = "/dev/shm"; }
            else if (!strncmp(tok, "shmmb=", 6)) { int v = atoi(tok + 6); if (v >= 16 && v <= 4096) shm_mb = v; }
            else if (!strncmp(tok, "ro=/", 4) && nro_extra < 3) ro_extra[nro_extra++] = strdup(tok + 3);
            else if (!strcmp(tok, "gfd=keep")) gpu_fd_keep = 1;
            else if (!strcmp(tok, "pidns=0")) no_pidns = 1;
            else if (!strcmp(tok, "noscope=1")) no_scope = 1; /* tests only: see no_scope */
            else if (!strcmp(tok, "rb=pbo")) rb_sync = 0;
            else if (!strcmp(tok, "rb=sync")) rb_sync = 1;
            else if (!strcmp(tok, "filter=nearest")) linear = 0;
        }
        free(o);
    }
    const char *corepath = argv[1], *rompath = argv[2], *sramp = argv[3]; sysdir = argv[4];
    out_w = (unsigned)atoi(argv[5]); out_h = (unsigned)atoi(argv[6]);
    if (out_w < 16 || out_h < 16 || out_w > 1920 || out_h > 1080) fail("bad size");
    if ((out_w | out_h) & 1) fail("size must be even");
    outbuf = calloc((size_t)out_w * out_h, 4); i420 = calloc((size_t)out_w * out_h * 3 / 2, 1);

    /* 1. the ROM goes to memory (cores that take data) and the save RAM too: the only disk access before the sandbox. A need_fullpath core
     *    (PSP: ISO/CSO of hundreds of MB to GB) is NOT read here: it opens the file itself, by path, from a directory that holds only that
     *    file and that the sandbox keeps readable (see step 3); the size is only taken with stat(). Whether the core wants a path is known
     *    only after dlopen(), so the ROM is read in step 2b (still before the sandbox and before any core function runs). */
    unsigned char *srambuf = NULL; size_t sramlen = 0;
    if (strcmp(sramp, "-")) {
        FILE *sf = fopen(sramp, "rb");
        if (sf) {
            srambuf = malloc(16u << 20); sramlen = srambuf ? fread(srambuf, 1, 16u << 20, sf) : 0; fclose(sf);
        }
    }

    /* 2. namespaces first (needs a single-threaded process), then load the library: its constructors may run, but
     *    no libretro function does, and RTLD_NOW resolves every symbol now so nothing is loaded lazily later */
    int flags = sandbox_netns();
    if (flags & FLAG_NETNS) flags |= enter_pidns(); /* from here on: the emulator (pid 1 of its namespace), or the same process if a namespace was not possible */
    if (want_shm && !(flags & FLAG_SHM)) fail("private /dev/shm could not be created (unprivileged user+mount namespaces needed)");
#ifdef HAVE_EGL
    if (want_gl_type) { /* GPU context BEFORE the sandbox (opens the GPU device nodes, loads the driver) */
        const char *e = gl_pre_init(want_gl_type, want_gl_major, want_gl_minor);
        if (e) fail(e);
        G.rb_sync = rb_sync; G.linear = linear; flags |= FLAG_GPU;
        if (dbg) fprintf(stderr, "GL: %s | %s\n", G.renderer, G.version);
    }
#else
    if (want_gl_type) fail("built without EGL");
#endif
    void *lib = dlopen(corepath, RTLD_NOW | RTLD_LOCAL);
    if (!lib) fail("core load failed");
#define SYM(v, n) do { *(void **)&(v) = dlsym(lib, n); if (!(v)) fail("core symbol missing: " n); } while (0)
    SYM(r_set_env, "retro_set_environment"); SYM(r_set_video, "retro_set_video_refresh"); SYM(r_set_asb, "retro_set_audio_sample_batch");
    SYM(r_set_as, "retro_set_audio_sample"); SYM(r_set_poll, "retro_set_input_poll"); SYM(r_set_input, "retro_set_input_state");
    SYM(r_init, "retro_init"); SYM(r_run, "retro_run"); SYM(r_deinit, "retro_deinit"); SYM(r_load, "retro_load_game");
    SYM(r_av, "retro_get_system_av_info"); SYM(r_sysinfo, "retro_get_system_info"); SYM(r_mem, "retro_get_memory_data"); SYM(r_memsize, "retro_get_memory_size");
    struct retro_system_info si; memset(&si, 0, sizeof(si)); r_sysinfo(&si); /* allowed before retro_init */

    /* 2b. the ROM: path-only cores are stat()ed, the others read into memory */
    long rsz = 0; void *rom = NULL;
    if (si.need_fullpath) {
        struct stat st; if (stat(rompath, &st) || !S_ISREG(st.st_mode) || st.st_size <= 0) fail("rom open failed");
        if (st.st_size > (16LL << 30)) fail("bad rom size"); /* the component's own ceiling is lower (PS2 images: 9 GiB by default) */
        rsz = (long)st.st_size;
    } else {
        FILE *f = fopen(rompath, "rb"); if (!f) fail("rom open failed");
        fseek(f, 0, SEEK_END); rsz = ftell(f); fseek(f, 0, SEEK_SET);
        if (rsz <= 0 || rsz > (128L << 20)) fail("bad rom size");
        rom = malloc((size_t)rsz); if (!rom || fread(rom, 1, (size_t)rsz, f) != (size_t)rsz) fail("rom read failed"); fclose(f);
    }

    /* 3. sandbox: seccomp + Landlock. Only the library dirs are readable; a need_fullpath core also gets the session
     *    directory (read-only), because it opens the ROM by path itself. */
    char sesdir[4096]; snprintf(sesdir, sizeof(sesdir), "%s", rompath);
    const char *ro[8] = { "/usr/lib", "/lib", "/usr/lib64", "/lib64", NULL };
    int nro = 4;
    if (si.need_fullpath) { ro[nro++] = dirname(sesdir); }
    for (int i = 0; i < nro_extra; i++) ro[nro++] = ro_extra[i];
    if (dbg) { fd_report("before");
#ifdef DEBUG
        fd_experiment();
#endif
    }
#ifdef HAVE_EGL
    if (want_gl_type) {
        int nc = close_gpu_fds();
        if (nc < 0) fail("cannot list /proc/self/fd to close the GPU descriptors");
        if (nc > 0) { const char *we = gl_warmup(); if (we) fail(we); } /* the context must still work with only the descriptors it needs */
        if (nc > 0) flags |= FLAG_GPU_FDS;
    }
#endif
    if (dbg) fd_report("after");
    close_sockets();
    { struct rlimit rl = { fsize, fsize }; if (fsize && setrlimit(RLIMIT_FSIZE, &rl)) fail("setrlimit(RLIMIT_FSIZE) failed"); }
    { int th = thread_count(); if (th != 1) { char m[96]; snprintf(m, sizeof m, "not single-threaded before the sandbox (Threads: %d)", th); fail(m); } }
    flags |= sandbox(ro, nro);

    /* 4. only now does core code run */
    r_set_env(env_cb); r_init();
    r_set_video(video_cb); r_set_asb(audio_batch_cb); r_set_as(audio_cb); r_set_poll(poll_cb); r_set_input(input_cb);
    struct retro_game_info gi = { si.need_fullpath ? rompath : NULL, si.need_fullpath ? NULL : rom, (size_t)rsz, NULL };
    if (!r_load(&gi)) fail("game load failed");
    struct retro_system_av_info av; r_av(&av);
#ifdef HAVE_EGL
    if (want_gl_type) {
        if (!hw_mode) fail("core did not ask for the GL context this session was set up for");
        const char *e = gl_make_targets(av.geometry.max_width, av.geometry.max_height, out_w, out_h);
        if (e) fail(e);
        free(G.ro); G.ro = (unsigned char *)outbuf; /* readback lands directly in the BGRA frame buffer */
        if (G.hw.context_reset) G.hw.context_reset();
    }
#endif
    size_t ssz = r_memsize(RETRO_MEMORY_SAVE_RAM); unsigned char *sram = ssz ? r_mem(RETRO_MEMORY_SAVE_RAM) : NULL;
    if (sram && srambuf) memcpy(sram, srambuf, sramlen < ssz ? sramlen : ssz);
    free(srambuf);
    unsigned char *lastsent = ssz ? calloc(1, ssz) : NULL; if (lastsent && sram) memcpy(lastsent, sram, ssz);

    unsigned char hello[36]; double fps = av.timing.fps > 1 ? av.timing.fps : 60.0, sr = av.timing.sample_rate > 1 ? av.timing.sample_rate : 32000;
    uint32_t w = out_w, h = out_h, s32 = (uint32_t)(sram ? ssz : 0), fl = (uint32_t)flags;
    memcpy(hello, &fps, 8); memcpy(hello + 8, &sr, 8); memcpy(hello + 16, &w, 4); memcpy(hello + 20, &h, 4); memcpy(hello + 24, &s32, 4); memcpy(hello + 28, &fl, 4);
    if (send_msg('H', hello, 32)) return 1;

    struct timespec next; clock_gettime(CLOCK_MONOTONIC, &next);
    long frame_ns = (long)(1e9 / fps); unsigned long frames = 0;
    for (;;) {
        if (drain_input() < 0 || want_quit) break;
        acount = 0; have_frame = 0;
        struct timespec t0; clock_gettime(CLOCK_MONOTONIC, &t0);
#ifdef HAVE_EGL
        if (hw_mode) gl_frame_begin();
#endif
        r_run();
        double run_ms = ms_since(&t0);
        struct timespec t1; clock_gettime(CLOCK_MONOTONIC, &t1);
        /* a duplicated frame (the core passes NULL: a 30 fps N64 game, FrameDuping) or one the PBO ring has not
         * delivered yet re-sends the last picture, so the stream stays at a steady 60 fps (an identical frame costs ~nothing in x264) */
        static int sent_one;
        if (have_frame) { to_i420(); sent_one = 1; }
        if (sent_one && send_msg('V', i420, out_w * out_h * 3 / 2)) return 1;
        double conv_ms = ms_since(&t1);
        /* per-stage timing for the parent: avg/max over 120 frames. core = retro_run minus (blit + readback) */
        static double acc[5], mx[5]; static unsigned nacc;
        double v[5] = { run_ms - t_blit_ms - t_read_ms, t_blit_ms, t_read_ms, conv_ms, run_ms + conv_ms };
        for (int k = 0; k < 5; k++) { acc[k] += v[k]; if (v[k] > mx[k]) mx[k] = v[k]; }
        if (++nacc == 120) {
            float t[12]; for (int k = 0; k < 5; k++) { t[k] = (float)(acc[k] / nacc); t[5 + k] = (float)mx[k]; acc[k] = mx[k] = 0; }
#ifdef HAVE_EGL
            t[10] = G.gpu_ms_n ? (float)(G.gpu_ms_sum / G.gpu_ms_n) : 0; G.gpu_ms_sum = 0; G.gpu_ms_n = 0;
#else
            t[10] = 0;
#endif
            t[11] = 0; nacc = 0;
            send_msg('T', t, sizeof t);
        }
        if (acount && send_msg('A', abuf, (uint32_t)(acount * 4))) return 1;
        if (sram && ++frames % 1800 == 0 && memcmp(sram, lastsent, ssz)) { memcpy(lastsent, sram, ssz); send_msg('S', sram, (uint32_t)ssz); }
        add_ns(&next, frame_ns);
        struct timespec now; clock_gettime(CLOCK_MONOTONIC, &now);
        long late = (now.tv_sec - next.tv_sec) * 1000000000L + (now.tv_nsec - next.tv_nsec);
        if (late > 4 * frame_ns) next = now; else clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &next, NULL);
    }
    if (sram) send_msg('S', sram, (uint32_t)ssz);
    send_msg('X', NULL, 0);
    return 0;
}
