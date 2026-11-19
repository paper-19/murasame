//go:build linux

package secretenv

import (
	"fmt"
	"syscall"
)

const prSetDumpable = 4

func HardenProcess() string {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetDumpable, 0, 0, 0, 0, 0); errno != 0 {
		return fmt.Sprintf("prctl(PR_SET_DUMPABLE) failed: %s", errno)
	}
	return "non-dumpable"
}
