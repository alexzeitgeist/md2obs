//go:build darwin

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// managedProcessIdentity returns the kernel-recorded process start time. It
// distinguishes a live daemon from an unrelated process that reused its PID.
func managedProcessIdentity(pid int) (string, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return "", errManagedProcessGone
		}
		return "", err
	}
	if process == nil || process.Proc.P_pid != int32(pid) {
		return "", errManagedProcessGone
	}
	started := process.Proc.P_starttime
	return fmt.Sprintf("%d:%d", started.Sec, started.Usec), nil
}

func signalManagedProcess(record managedWatchRecord) error {
	identity, err := managedProcessIdentity(record.PID)
	if err != nil {
		return err
	}
	if identity != record.ProcessIdentity {
		return errors.New("process identity changed; refusing to signal reused PID")
	}
	if err := unix.Kill(record.PID, unix.SIGTERM); errors.Is(err, unix.ESRCH) {
		return errManagedProcessGone
	} else {
		return err
	}
}
