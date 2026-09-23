//go:build !unix

package transcript

import "os"

// fileInode is 0 where the platform has no inode: the follower then tells a
// replaced file only by its size and content.
func fileInode(os.FileInfo) uint64 { return 0 }
