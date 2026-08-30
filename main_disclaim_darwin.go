//go:build darwin && cgo

package main

/*
#include <spawn.h>
#include <stdlib.h>
#include <unistd.h>

// These libSystem SPIs are the mechanism Chromium uses to give a spawned app
// an independent macOS responsibility identity.
int responsibility_spawnattrs_setdisclaim(posix_spawnattr_t *attrs, int disclaim);
pid_t responsibility_get_pid_responsible_for_pid(pid_t pid);

static char **ao_string_vector(size_t count) {
	return (char **)calloc(count + 1, sizeof(char *));
}

static void ao_string_vector_set(char **values, size_t index, char *value) {
	values[index] = value;
}

static int ao_exec_disclaimed(const char *path, char **argv, char **envp) {
	posix_spawnattr_t attrs;
	int err = posix_spawnattr_init(&attrs);
	if (err != 0) return err;
	short flags = 0;
	err = posix_spawnattr_getflags(&attrs, &flags);
	if (err == 0) err = posix_spawnattr_setflags(&attrs, flags | POSIX_SPAWN_SETEXEC);
	if (err == 0) err = responsibility_spawnattrs_setdisclaim(&attrs, 1);
	if (err == 0) {
		pid_t ignored = 0;
		err = posix_spawn(&ignored, path, NULL, &attrs, argv, envp);
	}
	posix_spawnattr_destroy(&attrs);
	return err;
}
*/
import "C"

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const harnessDisclaimEnv = "AO_HARNESS_DISCLAIM_RESPONSIBILITY"

// disclaimHarnessResponsibility replaces the windowed harness process in
// place, preserving its PID/stdout/process group while making it its own macOS
// responsible process. Without this, a harness launched from Agent Overflow's
// terminal inherits the developer app's responsibility set and its
// memory watchdog charges unrelated app, Codex, and browser processes to the
// harness. The marker is removed from the replacement environment, so this
// runs exactly once.
func disclaimHarnessResponsibility() error {
	if os.Getenv(harnessDisclaimEnv) != "1" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	env := make([]string, 0, len(os.Environ()))
	prefix := harnessDisclaimEnv + "="
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, prefix) {
			env = append(env, value)
		}
	}
	argv, freeArgv, err := cStringVector(os.Args)
	if err != nil {
		return err
	}
	defer freeArgv()
	envp, freeEnv, err := cStringVector(env)
	if err != nil {
		return err
	}
	defer freeEnv()
	path := C.CString(executable)
	defer C.free(unsafe.Pointer(path))
	if errno := C.ao_exec_disclaimed(path, argv, envp); errno != 0 {
		return fmt.Errorf("posix_spawn SETEXEC: %w", syscall.Errno(errno))
	}
	return fmt.Errorf("posix_spawn SETEXEC returned without replacing the process")
}

func cStringVector(values []string) (**C.char, func(), error) {
	vector := C.ao_string_vector(C.size_t(len(values)))
	if vector == nil {
		return nil, func() {}, fmt.Errorf("allocate posix_spawn string vector")
	}
	cstrings := make([]*C.char, 0, len(values))
	for i, value := range values {
		item := C.CString(value)
		cstrings = append(cstrings, item)
		C.ao_string_vector_set(vector, C.size_t(i), item)
	}
	return vector, func() {
		for _, item := range cstrings {
			C.free(unsafe.Pointer(item))
		}
		C.free(unsafe.Pointer(vector))
	}, nil
}

func currentResponsiblePID() int {
	return int(C.responsibility_get_pid_responsible_for_pid(C.pid_t(os.Getpid())))
}
