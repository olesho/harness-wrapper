package profile

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
)

// Plan is what a provisioner should do with one source profile tree. It is a
// description, not an action: this package copies nothing.
type Plan struct {
	// Provision lists files to copy AND record in the manifest. Sorted,
	// slash-separated, relative to the source root.
	Provision []string
	// Seed lists files to copy ONLY IF ABSENT at the destination, and never to
	// record in the manifest. Sorted.
	Seed []string
}

// BuildPlan walks a source profile tree and splits it into provisioned and
// seeded files for the harness.
//
// Everything under src is provisioned by default; the layout tables are the
// only exceptions. Seed files (top-level only) go to Seed, excluded directories
// are not walked at all, and junk (.DS_Store) is dropped entirely.
//
// An empty or absent tree yields an empty Plan and no error: whether a harness
// directory exists at all is the caller's opt-in signal, not an error here.
//
// A symlink or any other non-regular file is an ERROR naming the path, never a
// silent copy of its target. A symlinked profile file is exactly the kind of
// thing the fingerprint exists to notice, and following it would launder
// content past verification.
func BuildPlan(src fs.FS, h Harness) (Plan, error) {
	if err := h.Validate(); err != nil {
		return Plan{}, fmt.Errorf("%w: %q", err, string(h))
	}
	var plan Plan
	err := fs.WalkDir(src, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("profile: walk %s: %w", rel, err)
		}
		if rel == "." {
			return nil
		}
		name := path.Base(rel)
		if d.IsDir() {
			if h.IsExcludedDir(name) {
				return fs.SkipDir
			}
			return nil
		}
		if isJunk(name) || name == ManifestName {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("profile: %s: %s is not a regular file; symlinks and "+
				"special files are never provisioned", rel, d.Type())
		}
		if h.IsSeed(rel) {
			plan.Seed = append(plan.Seed, rel)
			return nil
		}
		plan.Provision = append(plan.Provision, rel)
		return nil
	})
	if err != nil {
		return Plan{}, err
	}
	sort.Strings(plan.Provision)
	sort.Strings(plan.Seed)
	return plan, nil
}

// BuildManifest hashes an ALREADY-WRITTEN destination and returns the manifest
// to store at <configRoot>/.manifest.json.
//
// files is sorted and de-duplicated first, so the manifest's list is always in
// the one legal order, and the fingerprint is computed over that same order.
// harnessVersion is the trimmed first line of `<binary> --version`; observing
// it is the caller's job, and an empty one is rejected — an unpinned manifest
// could never drift-check.
func BuildManifest(fsys fs.FS, files []string, harnessVersion string) (Manifest, error) {
	sorted := cloneList(files)
	sort.Strings(sorted)
	sorted = dedupeSorted(sorted)

	sum, err := Fingerprint(fsys, sorted)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Files:          sorted,
		Fingerprint:    sum,
		HarnessVersion: harnessVersion,
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// dedupeSorted removes adjacent duplicates from an already-sorted slice.
func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
