//go:build unix

package transcript

import (
	"os"
	"syscall"
)

// fileInode is the file's inode number, which a rotation (a new file renamed
// into place) changes. Only the inode is kept, not the device: device numbers
// are not stable across reboots and remounts on every filesystem (btrfs
// subvolumes, overlayfs, NFS), and the path already pins the filesystem.
func fileInode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is not uint64 on every unix
	}
	return 0
}
