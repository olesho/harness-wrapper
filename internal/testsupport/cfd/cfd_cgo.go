//go:build linux && cgo

// Package cfd opens file descriptors from C for the containment tests: files
// and listeners without close-on-exec, and C threads that keep opening them,
// none of which Go's ForkLock knows about. Only tests import it, so no library
// build compiles its C code.
package cfd

/*
#include <fcntl.h>
#include <netinet/in.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static int cfd_open_noncloexec(const char *path) { return open(path, O_RDONLY); }

static int cfd_listen_noncloexec(void) {
	int s = socket(AF_INET, SOCK_STREAM, 0);
	if (s < 0) return -1;
	struct sockaddr_in a;
	memset(&a, 0, sizeof a);
	a.sin_family = AF_INET;
	a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
	if (bind(s, (struct sockaddr *)&a, sizeof a) < 0 || listen(s, 1) < 0) { close(s); return -1; }
	return s;
}

static atomic_int cfd_stop;
static atomic_long cfd_opens;
static char cfd_path[4096];
static pthread_t cfd_threads[16];
static int cfd_nthreads;

static void *cfd_opener(void *arg) {
	(void)arg;
	while (!atomic_load(&cfd_stop)) {
		int fd = open(cfd_path, O_RDONLY); // no O_CLOEXEC, racing every fork
		if (fd >= 0) { atomic_fetch_add(&cfd_opens, 1); close(fd); }
	}
	return NULL;
}

static int cfd_start_openers(const char *path, int n) {
	strncpy(cfd_path, path, sizeof cfd_path - 1);
	atomic_store(&cfd_stop, 0);
	if (n > 16) n = 16;
	int started = 0;
	for (int i = 0; i < n; i++) {
		if (pthread_create(&cfd_threads[i], NULL, cfd_opener, NULL) != 0) break;
		started++;
	}
	cfd_nthreads = started;
	return started;
}

static long cfd_stop_openers(void) {
	atomic_store(&cfd_stop, 1);
	for (int i = 0; i < cfd_nthreads; i++) pthread_join(cfd_threads[i], NULL);
	cfd_nthreads = 0;
	return atomic_load(&cfd_opens);
}
*/
import "C"

import "unsafe"

// Available reports whether this build can open descriptors from C.
const Available = true

// OpenNonCloexec opens path read-only from C, without O_CLOEXEC.
func OpenNonCloexec(path string) int {
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	return int(C.cfd_open_noncloexec(cs))
}

// ListenNonCloexec opens a loopback TCP listener from C, without
// SOCK_CLOEXEC.
func ListenNonCloexec() int { return int(C.cfd_listen_noncloexec()) }

// Close closes fd from C.
func Close(fd int) { C.close(C.int(fd)) }

// StartOpeners starts n C threads that open and close path without
// O_CLOEXEC until StopOpeners; it returns how many started.
func StartOpeners(path string, n int) int {
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	return int(C.cfd_start_openers(cs, C.int(n)))
}

// StopOpeners stops the C openers and returns how many opens they made.
func StopOpeners() int64 { return int64(C.cfd_stop_openers()) }
