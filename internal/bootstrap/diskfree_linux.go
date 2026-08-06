//go:build linux

package bootstrap

import "syscall"

// FreeBytes reports the space available at a path, or -1 when it cannot be
// determined.
//
// Deliberately returns the space available to an *unprivileged* process rather
// than the raw free count. Filesystems reserve a percentage for root, and the
// daemon does not run as root — so counting the reserve would let a download
// start with room that this process is not allowed to use, and fail near the end
// of a nine-gigabyte transfer.
func FreeBytes(path string) int64 {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return -1
	}
	// Bavail counts blocks and is unsigned; Bsize is already signed. The product
	// is a byte count, which fits in int64 for any device that exists.
	//nolint:gosec // a free-block count cannot approach the top of an int64.
	return int64(fs.Bavail) * fs.Bsize
}
