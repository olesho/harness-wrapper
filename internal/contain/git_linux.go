//go:build linux

package contain

import (
	"os"
	"path/filepath"
	"strings"
)

// gitMetadataNotes reports the Git metadata a working directory depends on
// that no grant covers. A linked worktree's .git file points at its gitdir
// (inside the primary checkout's .git/worktrees) and at the common directory
// holding objects and refs; a working directory below a repository's root
// needs the root's .git. None of it is granted automatically: sharing the
// primary checkout's metadata is the caller's decision, made with
// --contain-rw (or -ro).
func gitMetadataNotes(wd string, grants []*pinned) []string {
	covered := func(path string) bool {
		p, err := pinPath(path)
		if err != nil {
			return true // absent: nothing to grant
		}
		defer p.close()
		for _, g := range grants {
			if g.contains(p) {
				return true
			}
		}
		return false
	}
	var notes []string
	for dir := wd; ; dir = filepath.Dir(dir) {
		dotgit := filepath.Join(dir, ".git")
		fi, err := os.Lstat(dotgit)
		if err == nil {
			switch {
			case fi.Mode().IsRegular():
				b, _ := os.ReadFile(dotgit)
				gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
				if !ok {
					break
				}
				if !filepath.IsAbs(gitdir) {
					gitdir = filepath.Join(dir, gitdir)
				}
				if !covered(gitdir) {
					notes = append(notes, "Git worktree metadata "+gitdir+" is not granted; git in "+wd+" will fail without it (grant it with --contain-rw)")
				}
				if c, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
					common := strings.TrimSpace(string(c))
					if !filepath.IsAbs(common) {
						common = filepath.Join(gitdir, common)
					}
					if common = filepath.Clean(common); !covered(common) {
						notes = append(notes, "the repository's common Git directory "+common+" (objects, refs) is not granted; granting it shares the primary checkout's repository with the harness")
					}
				}
			case fi.IsDir() && !covered(dotgit):
				notes = append(notes, "the repository metadata "+dotgit+" above the working directory is not granted; git will not find the repository (grant it with --contain-rw)")
			}
			break
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return notes
}
