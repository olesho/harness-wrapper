//go:build linux

package contain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// objectID identifies a filesystem object independently of the path used to
// reach it, so bind mounts and symlinks cannot make one object look like two.
type objectID struct {
	dev uint64
	ino uint64
}

// pinned is a path resolved once, in the kernel, to the object it named at
// that moment. Every later check and the Landlock rule itself use this
// descriptor — never the pathname again — so a concurrent symlink or ancestor
// replacement can at worst make the checks fail, never redirect the grant to
// the replacement.
type pinned struct {
	fd        int
	requested string // the path as written
	canonical string // the kernel's name for the object when it was pinned
	id        objectID
	mode      uint32 // S_IFMT bits
	fsType    int64
	// ancestors are the objects above this one, nearest first, as walked
	// through ".." from the pinned object (for a non-directory, from the
	// directory that contained it when it was pinned). Checked by identity.
	ancestors []objectID
}

func (p *pinned) isDir() bool { return p.mode == unix.S_IFDIR }

func (p *pinned) close() {
	if p != nil && p.fd >= 0 {
		_ = unix.Close(p.fd)
		p.fd = -1
	}
}

// errNotFound is wrapped when a pinned path does not exist, so callers can
// tell an absent optional path from a real failure.
var errNotFound = errors.New("does not exist")

// pinPath resolves path — following symlinks, but no /proc magic links —
// with openat2 and returns the pinned object. The canonical name comes from
// the kernel (/proc/self/fd), not from a separate string walk.
func pinPath(path string) (*pinned, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%q is not an absolute path", path)
	}
	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &how)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%q %w", path, errNotFound)
		}
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	p, err := describePinned(fd, path)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return p, nil
}

// pinAt pins name relative to the pinned directory dir, following no symlink
// at all (ELOOP if name is one): used for objects the wrapper creates itself.
func pinAt(dir *pinned, name string) (*pinned, error) {
	how := unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(dir.fd, name, &how)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%q %w", filepath.Join(dir.canonical, name), errNotFound)
		}
		return nil, fmt.Errorf("open %q: %w", filepath.Join(dir.canonical, name), err)
	}
	p, err := describePinned(fd, filepath.Join(dir.canonical, name))
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return p, nil
}

// pinFD takes ownership of an already-open descriptor (the session terminal).
func pinFD(fd int, requested string) (*pinned, error) {
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, fmt.Errorf("dup %s: %w", requested, err)
	}
	p, err := describePinned(dup, requested)
	if err != nil {
		_ = unix.Close(dup)
		return nil, err
	}
	return p, nil
}

func describePinned(fd int, requested string) (*pinned, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("stat %q: %w", requested, err)
	}
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &sfs); err != nil {
		return nil, fmt.Errorf("statfs %q: %w", requested, err)
	}
	canonical, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return nil, fmt.Errorf("canonical name of %q: %w", requested, err)
	}
	p := &pinned{
		fd:        fd,
		requested: requested,
		canonical: canonical,
		id:        objectID{dev: uint64(st.Dev), ino: st.Ino},
		mode:      st.Mode & unix.S_IFMT,
		fsType:    int64(sfs.Type),
	}
	if err := p.walkAncestors(); err != nil {
		return nil, err
	}
	return p, nil
}

// walkAncestors records the identity of every directory above the object,
// walking ".." from the object itself (a directory) or from the directory the
// kernel names as its parent (a non-directory, which has no ".."). For the
// non-directory case the parent is pinned by name and then verified to still
// contain the same object; a mismatch means the path changed under us, and the
// grant is refused rather than trusted.
func (p *pinned) walkAncestors() error {
	var start int
	if p.isDir() {
		fd, err := unix.Openat(p.fd, "..", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("parent of %q: %w", p.canonical, err)
		}
		start = fd
	} else {
		parent := filepath.Dir(p.canonical)
		how := unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS}
		fd, err := unix.Openat2(unix.AT_FDCWD, parent, &how)
		if err != nil {
			return fmt.Errorf("parent of %q: %w", p.canonical, err)
		}
		var st unix.Stat_t
		err = unix.Fstatat(fd, filepath.Base(p.canonical), &st, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil || uint64(st.Dev) != p.id.dev || st.Ino != p.id.ino {
			_ = unix.Close(fd)
			return fmt.Errorf("%q changed while it was being resolved; refusing to grant it", p.requested)
		}
		start = fd
	}

	cur := start
	defer func() { _ = unix.Close(cur) }()
	for depth := 0; depth < 4096; depth++ {
		var st unix.Stat_t
		if err := unix.Fstat(cur, &st); err != nil {
			return fmt.Errorf("ancestor of %q: %w", p.canonical, err)
		}
		id := objectID{dev: uint64(st.Dev), ino: st.Ino}
		if n := len(p.ancestors); n > 0 && p.ancestors[n-1] == id {
			return nil // reached the root: ".." of "/" is "/"
		}
		p.ancestors = append(p.ancestors, id)
		next, err := unix.Openat(cur, "..", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("ancestor of %q: %w", p.canonical, err)
		}
		_ = unix.Close(cur)
		cur = next
	}
	return fmt.Errorf("ancestry of %q is deeper than 4096 levels", p.canonical)
}

// contains reports whether object other is p itself or lies beneath p.
func (p *pinned) contains(other *pinned) bool {
	if p.id == other.id {
		return true
	}
	for _, a := range other.ancestors {
		if a == p.id {
			return true
		}
	}
	return false
}

// overlaps reports whether either object lies beneath (or is) the other.
func (p *pinned) overlaps(other *pinned) bool {
	return p.contains(other) || other.contains(p)
}
