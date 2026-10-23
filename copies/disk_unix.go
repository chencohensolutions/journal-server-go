//go:build unix

package copies

import (
	"fmt"
	"os"
	"syscall"
)

// sameDisk reports whether a and b are on one file system (the same device).
func sameDisk(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	sa, oka := ia.Sys().(*syscall.Stat_t)
	sb, okb := ib.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return false, fmt.Errorf("no device numbers for %s and %s", a, b)
	}
	return sa.Dev == sb.Dev, nil
}
