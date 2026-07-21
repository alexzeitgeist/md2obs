//go:build !linux && !darwin

package main

import (
	"errors"
	"os/exec"
)

func configureDaemonProcess(*exec.Cmd) error {
	return errors.New("managed watch commands are supported only on Linux and macOS")
}
