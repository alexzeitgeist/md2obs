//go:build !linux && !darwin

package main

import (
	"context"
	"errors"
	"time"

	"md2obs/internal/config"
)

var errManagedWatchUnsupported = errors.New("managed watch commands are supported only on Linux and macOS")

func claimManagedWatch(*config.Config, managedWatchSettings) (managedWatchRecord, func(), error) {
	return managedWatchRecord{}, nil, errManagedWatchUnsupported
}

func inspectManagedWatch(*config.Config) (managedWatchState, error) {
	return managedWatchState{Unsupported: true}, nil
}

func stopManagedWatch(context.Context, *config.Config, time.Duration) (managedWatchRecord, bool, error) {
	return managedWatchRecord{}, false, errManagedWatchUnsupported
}
