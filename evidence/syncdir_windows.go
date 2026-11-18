//go:build windows

package evidence

// syncDir is a no-op on Windows: opening a directory read-only and calling Sync
// fails with "Access is denied" (ERROR_ACCESS_DENIED), because Win32 has no
// fsync-for-directories. The rename itself is already durable through the file
// system journal, so the barrier below is a POSIX-only refinement; failing the
// whole write here would make evidence capture unusable on Windows, which is the
// worse trade.
func syncDir(string) error { return nil }
