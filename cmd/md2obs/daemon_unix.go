//go:build linux || darwin

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

func configureDaemonProcess(cmd *exec.Cmd) error {
	if cmd == nil {
		return fmt.Errorf("configure watch daemon: nil command")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
