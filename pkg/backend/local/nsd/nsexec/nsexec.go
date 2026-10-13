//go:build linux && cgo

// Package nsexec is the part of silo-nsd that runs before the Go
// runtime starts (docs/silo/local-rootless-storage.md, call L27).
// setns(2) into a user namespace is refused to a multi-threaded
// process, which every Go program is by the time main runs, so the
// namespace work happens in a C constructor, as runc's nsexec does.
//
// A binary that wants it imports this package for its side effect:
//
//	import _ "github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
//
// The constructor does nothing unless _SILO_NSEXEC is set, and it
// unsets that variable before Go's main runs. The stages are:
//
//   - create: unshare a user namespace, write "u" on the descriptor in
//     _SILO_NSEXEC_SYNC and wait for "m" (the parent has run newuidmap
//     and newgidmap), become root in it, then unshare a mount namespace
//     with private propagation and a network namespace: the holder.
//   - join: enter the user, mount and network namespaces of the process
//     in _SILO_NSEXEC_PID, as root of its user namespace: silo unshare,
//     and anything else run "inside the holder".
//   - pause: block forever. The holder starts one per container in a
//     fresh nested user namespace and netns, pins both by bind mounts,
//     and kills it.
package nsexec

/*
#cgo CFLAGS: -Wall
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <unistd.h>

static void silo_die(const char *what) {
	fprintf(stderr, "silo nsexec: %s: %s\n", what, strerror(errno));
	_exit(125);
}

static void silo_become_root(void) {
	if (setresgid(0, 0, 0) < 0) silo_die("setresgid");
	if (setgroups(0, NULL) < 0 && errno != EPERM) silo_die("setgroups");
	if (setresuid(0, 0, 0) < 0) silo_die("setresuid");
}

static void silo_create(void) {
	const char *s = getenv("_SILO_NSEXEC_SYNC");
	if (s == NULL) { errno = EINVAL; silo_die("_SILO_NSEXEC_SYNC"); }
	int fd = atoi(s);
	char c = 'u';
	if (unshare(CLONE_NEWUSER) < 0) silo_die("unshare user namespace");
	if (write(fd, &c, 1) != 1) silo_die("sync write");
	if (read(fd, &c, 1) != 1 || c != 'm') { errno = EPROTO; silo_die("waiting for newuidmap"); }
	close(fd);
	silo_become_root();
	if (unshare(CLONE_NEWNS | CLONE_NEWNET) < 0) silo_die("unshare mount and network namespaces");
	if (mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL) < 0) silo_die("make / private");
}

static void silo_join(void) {
	const char *pid = getenv("_SILO_NSEXEC_PID");
	static const char *kinds[] = {"user", "mnt", "net"};
	int fds[3];
	char path[64], cwd[4096];
	if (pid == NULL) { errno = EINVAL; silo_die("_SILO_NSEXEC_PID"); }
	// Open all three first: /proc is the host's until the mount
	// namespace changes.
	for (int i = 0; i < 3; i++) {
		snprintf(path, sizeof path, "/proc/%s/ns/%s", pid, kinds[i]);
		if ((fds[i] = open(path, O_RDONLY | O_CLOEXEC)) < 0) silo_die(path);
	}
	if (getcwd(cwd, sizeof cwd) == NULL) cwd[0] = 0;
	if (setns(fds[0], CLONE_NEWUSER) < 0) silo_die("setns user");
	silo_become_root();
	if (setns(fds[1], CLONE_NEWNS) < 0) silo_die("setns mnt");
	if (setns(fds[2], CLONE_NEWNET) < 0) silo_die("setns net");
	for (int i = 0; i < 3; i++) close(fds[i]);
	if (cwd[0] == 0 || chdir(cwd) < 0) (void)chdir("/");
}

__attribute__((constructor)) static void silo_nsexec(void) {
	const char *stage = getenv("_SILO_NSEXEC");
	if (stage == NULL) return;
	if (strcmp(stage, "create") == 0) silo_create();
	else if (strcmp(stage, "join") == 0) silo_join();
	else if (strcmp(stage, "pause") == 0) for (;;) pause();
	else { errno = EINVAL; silo_die(stage); }
	unsetenv("_SILO_NSEXEC");
	unsetenv("_SILO_NSEXEC_SYNC");
	unsetenv("_SILO_NSEXEC_PID");
}
*/
import "C"

// Available reports whether this binary carries the constructor.
const Available = true
