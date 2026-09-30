package saveload

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// manifestName is the archive's own entry: what was saved, and from where.
const manifestName = "saveload/manifest.json"

// entry is one saved file: where it was beneath its root, and what it held.
type entry struct {
	Root   contract.Root `json:"root"`
	Path   string        `json:"path"` // beneath the root, slash separated
	Size   int64         `json:"size"`
	SHA256 string        `json:"sha256"`
}

func (e entry) name() string { return string(e.Root) + "/" + e.Path }

// manifest says what an archive holds and what a restore needs of the source,
// which is gone by then: the harness and its version, the Session's native id,
// and the source's roots. Workspace is the source workspace as the harness saw
// it — resolved — because a harness that keys its history by the workspace
// keys it by that path, and nothing at the destination can resolve a path of
// a machine that is gone.
type manifest struct {
	Harness   contract.HarnessInfo `json:"harness"`
	SessionID string               `json:"session_id"`
	Layout    contract.Layout      `json:"layout"`
	Workspace string               `json:"workspace"`
	Variant   string               `json:"variant"`
	Entries   []entry              `json:"entries"`
}

// relocation maps the saved files beneath one path to another: a prefix rule,
// the form the plan gives ProvisionResult.history_relocations. A file no rule
// covers keeps its root and path.
type relocation struct {
	From contract.RootPath `json:"from"`
	To   contract.RootPath `json:"to"`
}

// relocate is where a saved file goes: by the first rule whose prefix holds
// it.
func relocate(rules []relocation, e entry) contract.RootPath {
	for _, r := range rules {
		if e.Root != r.From.Root {
			continue
		}
		if e.Path == r.From.Path {
			return r.To
		}
		if rest, ok := strings.CutPrefix(e.Path, r.From.Path+"/"); ok {
			return contract.RootPath{Root: r.To.Root, Path: path.Join(r.To.Path, rest)}
		}
	}
	return contract.RootPath{Root: e.Root, Path: e.Path}
}

// tree lists the regular files beneath a root of layout l, hashed. Anything
// else — a symlink, a socket — is reported, never followed.
func tree(l contract.Layout, root contract.Root) (files []entry, other []string, err error) {
	base := l.Path(root)
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			other = append(other, string(root)+"/"+rel+" ("+d.Type().String()+")")
			return nil
		}
		sum, size, herr := hashFile(p, -1)
		if herr != nil {
			return herr
		}
		files = append(files, entry{Root: root, Path: rel, Size: size, SHA256: sum})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, other, err
}

// hashFile is the SHA-256 of a file's first limit bytes, or of all of it when
// limit is negative, and how many bytes that was.
func hashFile(name string, limit int64) (string, int64, error) {
	f, err := os.Open(name) //nolint:gosec // a path of the probe's own environment
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if limit >= 0 {
		r = io.LimitReader(f, limit)
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// treeDigest is one digest of a directory tree: every file's path, type, size
// and content, in order. A tree that was read, and not written, keeps it.
func treeDigest(base string) (string, int, error) {
	h := sha256.New()
	n := 0
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(base, p)
		switch {
		case d.IsDir():
			_, _ = fmt.Fprintf(h, "d %s\n", rel)
		case d.Type().IsRegular():
			sum, size, err := hashFile(p, -1)
			if err != nil {
				return err
			}
			n++
			_, _ = fmt.Fprintf(h, "f %s %d %s\n", rel, size, sum)
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			_, _ = fmt.Fprintf(h, "l %s %s\n", rel, target)
		default:
			_, _ = fmt.Fprintf(h, "o %s %s\n", rel, d.Type())
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// writeArchive copies the manifest's entries out of the source layout into a
// standalone tar at name: each file under <root>/<path>, as agentd's archives
// name them, and the manifest. It fails if a file no longer holds what the
// manifest says: a harness still writing.
func writeArchive(name string, m manifest) (err error) {
	out, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a path of the probe's own work directory
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(out)
	for _, e := range m.Entries {
		data, rerr := os.ReadFile(filepath.Join(m.Layout.Path(e.Root), filepath.FromSlash(e.Path))) //nolint:gosec // a path of the probe's own environment
		if rerr != nil {
			return rerr
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != e.SHA256 {
			return fmt.Errorf("%s changed while it was being saved", e.name())
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name(), Size: int64(len(data)), Mode: 0o600, Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	meta, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: manifestName, Size: int64(len(meta)), Mode: 0o600, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := tw.Write(meta); err != nil {
		return err
	}
	return tw.Close()
}

// readManifest reads an archive's manifest.
func readManifest(name string) (manifest, error) {
	var m manifest
	err := eachEntry(name, func(h *tar.Header, r io.Reader) error {
		if h.Name != manifestName {
			return nil
		}
		return json.NewDecoder(r).Decode(&m)
	})
	if err == nil && m.SessionID == "" {
		err = errors.New("the archive has no manifest")
	}
	return m, err
}

func eachEntry(name string, f func(*tar.Header, io.Reader) error) error {
	in, err := os.Open(name) //nolint:gosec // a path of the probe's own work directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tr := tar.NewReader(in)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := f(h, tr); err != nil {
			return err
		}
	}
}

// restore writes an archive's files into layout l, each where the relocation
// rules put it, and returns where each went. It reads the archive alone: the
// manifest names the files, the tar holds them, and nothing of the source is
// opened. A file is written beneath its root without following a link, as the
// workload's: 0600, in directories 0700. It refuses an entry the manifest does
// not list, one whose content differs from it, and two that land on one path.
func restore(name string, l contract.Layout, rules []relocation) (map[string]contract.RootPath, error) {
	m, err := readManifest(name)
	if err != nil {
		return nil, err
	}
	listed := map[string]entry{}
	for _, e := range m.Entries {
		listed[e.name()] = e
	}
	placed := map[string]contract.RootPath{}
	taken := map[contract.RootPath]string{}
	err = eachEntry(name, func(h *tar.Header, r io.Reader) error {
		if h.Name == manifestName {
			return nil
		}
		e, ok := listed[h.Name]
		if !ok || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s: not a file the manifest lists", h.Name)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != e.SHA256 || int64(len(data)) != e.Size {
			return fmt.Errorf("%s: does not hold what the manifest says", h.Name)
		}
		to := relocate(rules, e)
		if err := contract.CheckRelPath(to.Path); err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		if prev, dup := taken[to]; dup {
			return fmt.Errorf("%s and %s both land on %s/%s", prev, h.Name, to.Root, to.Path)
		}
		taken[to] = h.Name
		dir := l.Path(to.Root)
		if dir == "" {
			return fmt.Errorf("%s: no root %q", h.Name, to.Root)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		if parent := path.Dir(to.Path); parent != "." {
			if err := root.MkdirAll(filepath.FromSlash(parent), 0o700); err != nil {
				return err
			}
		}
		if err := root.WriteFile(filepath.FromSlash(to.Path), data, 0o600); err != nil {
			return err
		}
		placed[h.Name] = to
		return nil
	})
	if err == nil && len(placed) != len(m.Entries) {
		err = fmt.Errorf("the archive holds %d of the manifest's %d files", len(placed), len(m.Entries))
	}
	return placed, err
}
