package util

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"go.uber.org/zap"
)

// EnsureDirExists creates the given directory path if it doesn't already exist
func EnsureDirExists(path string) error {
	if err := os.MkdirAll(path, os.ModePerm); err != nil {
		return fmt.Errorf("ensure directory exists (%s): %w", path, err)
	}

	return nil
}

// FileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// Linux returns true if we're running on Linux
func Linux() bool {
	return runtime.GOOS == "linux"
}

// SetupCloseHandler creates a listener that notifies the program of OS interrupt signals.
func SetupCloseHandler() chan os.Signal {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	return c
}

// OpenExternal spawns a process with the provided command and argument
func OpenExternal(logger *zap.SugaredLogger, cmd string, arg string) error {
	command := exec.Command(cmd, arg)

	if err := command.Run(); err != nil {
		logger.Warnw("Failed to spawn process",
			"command", cmd,
			"argument", arg,
			"error", err)

		return fmt.Errorf("spawn process: %w", err)
	}

	return nil
}

// NormalizeScalar rounds the given float32 to 2 points of precision (e.g. 0.15442 -> 0.15).
// It rounds instead of truncating, since float32 values like 0.9 are stored as
// 0.89999998 and would otherwise come out as 0.89.
func NormalizeScalar(v float32) float32 {
	return float32(math.Round(float64(v)*100) / 100.0)
}

// SignificantlyDifferent returns true if there's a significant enough volume difference between two given values
func SignificantlyDifferent(old float32, new float32, noiseReductionLevel string) bool {

	const (
		noiseReductionHigh = "high"
		noiseReductionLow  = "low"
	)

	var significantDifferenceThreshold float64

	switch noiseReductionLevel {
	case noiseReductionHigh:
		significantDifferenceThreshold = 0.035
	case noiseReductionLow:
		significantDifferenceThreshold = 0.015
	default:
		significantDifferenceThreshold = 0.025
	}

	if math.Abs(float64(old-new)) >= significantDifferenceThreshold {
		return true
	}

	// special behavior is needed around the edges of 0.0 and 1.0 - this makes it snap to them
	if (almostEquals(new, 1.0) && old != 1.0) || (almostEquals(new, 0.0) && old != 0.0) {
		return true
	}

	return false
}

func almostEquals(a float32, b float32) bool {
	return math.Abs(float64(a-b)) < 0.000001
}
