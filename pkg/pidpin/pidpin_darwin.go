//go:build darwin && cgo

package pidpin

/*
#include <libproc.h>
#include <mach/mach.h>

static kern_return_t token_of(int pid, audit_token_t *token) {
	mach_port_t name;
	kern_return_t kr = task_name_for_pid(mach_task_self(), pid, &name);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	mach_msg_type_number_t n = TASK_AUDIT_TOKEN_COUNT;
	kr = task_info(name, TASK_AUDIT_TOKEN, (task_info_t)token, &n);
	kern_return_t dr = mach_port_deallocate(mach_task_self(), name);
	if (kr != KERN_SUCCESS) {
		return kr;
	}
	return dr;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB and exiting is P_WEXIT in sys/proc.h, which x/sys does not name.
const (
	zombie  = 5
	exiting = 0x2000
)

// pidVersion is the audit token word that tells two holders of one pid apart.
const pidVersion = 7

// handle is the audit token: the kernel signals through it only while its pid version matches the process on the pid.
type handle struct {
	token [8]uint32
}

func open(pid int) (handle, error) {
	var token C.audit_token_t
	if kr := C.token_of(C.int(pid), &token); kr != C.KERN_SUCCESS {
		// task_name_for_pid fails alike for a pid nobody holds and for a zombie, which no signal reaches either.
		if exited(pid) {
			return handle{}, syscall.ESRCH
		}
		return handle{}, fmt.Errorf("task_name_for_pid: %s", C.GoString(C.mach_error_string(kr)))
	}
	var h handle
	for i := range h.token {
		h.token[i] = uint32(token.val[i])
	}

	return h, nil
}

// exited says no live process holds pid: none does, or a zombie does.
func exited(pid int) bool {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)

	return err == nil && (len(procs) == 0 || procs[0].Proc.P_stat == zombie)
}

// leaving says the process on pid has begun its exit, whose task refuses a token before it turns zombie.
func leaving(pid int) bool {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)

	return err == nil && len(procs) > 0 && procs[0].Proc.P_flag&exiting != 0
}

func signal(h handle, sig syscall.Signal) error {
	var token C.audit_token_t
	for i, v := range h.token {
		token.val[i] = C.uint(v)
	}
	if rc := C.proc_signal_with_audittoken(&token, C.int(sig)); rc != 0 {
		return syscall.Errno(rc)
	}

	return nil
}

// gone compares the pid version of whoever holds pid now with the token's, since a token outlives its process.
func gone(h handle, pid int) (bool, error) {
	if exited(pid) {
		return true, nil
	}
	now, err := open(pid)
	if errors.Is(err, syscall.ESRCH) || (err != nil && leaving(pid)) {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	return now.token[pidVersion] != h.token[pidVersion], nil
}

func release(handle) error { return nil }
