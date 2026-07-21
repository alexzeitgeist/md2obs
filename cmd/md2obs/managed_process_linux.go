//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// managedProcessIdentity returns Linux's kernel process start tick. Unlike a
// wall-clock timestamp, this value comes from the same process table entry as
// the PID and changes when a PID is reused.
func managedProcessIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", errWatchProcessGone
	}
	if err != nil {
		return "", err
	}
	// The command field is parenthesized and may itself contain spaces or ')'.
	// Fields after its final ") " begin at field 3; starttime is field 22.
	endCommand := strings.LastIndex(string(data), ") ")
	if endCommand < 0 {
		return "", errors.New("malformed /proc stat command field")
	}
	fields := strings.Fields(string(data)[endCommand+2:])
	if len(fields) <= 19 {
		return "", errors.New("malformed /proc stat start-time field")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", fmt.Errorf("parse /proc start time %q: %w", fields[19], err)
	}
	return fields[19], nil
}

func signalManagedProcess(record managedWatchRecord) error {
	pidfd, err := unix.PidfdOpen(record.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return errWatchProcessGone
	}
	if err == nil {
		defer unix.Close(pidfd)
		identity, identityErr := managedProcessIdentity(record.PID)
		if identityErr != nil {
			return identityErr
		}
		if identity != record.ProcessIdentity {
			return errors.New("process identity changed; refusing to signal reused PID")
		}
		if err := unix.PidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); errors.Is(err, unix.ESRCH) {
			return errWatchProcessGone
		} else {
			return err
		}
	}

	// pidfd_open was added in Linux 5.3. Retain support for older kernels by
	// checking the start marker immediately before the traditional kill call.
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	identity, err := managedProcessIdentity(record.PID)
	if err != nil {
		return err
	}
	if identity != record.ProcessIdentity {
		return errors.New("process identity changed; refusing to signal reused PID")
	}
	if err := unix.Kill(record.PID, unix.SIGTERM); errors.Is(err, unix.ESRCH) {
		return errWatchProcessGone
	} else {
		return err
	}
}
