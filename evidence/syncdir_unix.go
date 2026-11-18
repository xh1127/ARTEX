//go:build !windows

package evidence

import "os"

// syncDir flushes a directory entry so a completed rename survives a crash.
// Supported on POSIX: opening a directory read-only and fsyncing it persists the
// link change. Errors are propagated — silently degrading would turn a durability
// guarantee into a coin flip.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
