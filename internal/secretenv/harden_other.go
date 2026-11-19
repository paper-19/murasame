//go:build !linux

package secretenv

import "runtime"

func HardenProcess() string {
	return "not available on " + runtime.GOOS + ": a gate running as this user can read this process's environment"
}
