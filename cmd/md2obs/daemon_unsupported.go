//go:build !linux && !darwin

package main

import (
	"errors"
	"os/exec"
)

func configureDaemonProcess(*exec.Cmd) error {
	return errors.New("--daemon is supported only on Linux and macOS")
}
