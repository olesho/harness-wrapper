//go:build linux

package contain

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// install is a harness executable identified against its profile.
type install struct {
	// exe is the pinned executable; path is its canonical name, which the
	// child executes.
	exe  *pinned
	path string
	// grants are the read/execute trees the executable needs besides itself:
	// codex's package root and node, a test profile's directories.
	grants []*pinned
}

func (in *install) close() {
	in.exe.close()
	for _, g := range in.grants {
		g.close()
	}
}

// errBinaryNotFound carries exec.LookPath's failure through unchanged so the
// wrapper reports a missing binary as ErrBinaryNotFound on this path too.
type errBinaryNotFound struct{ err error }

func (e *errBinaryNotFound) Error() string { return e.err.Error() }
func (e *errBinaryNotFound) Unwrap() error { return e.err }

// identifyExecutable resolves binaryPath the way exec.Command does, pins the
// result and checks it against the profile. An unknown layout or version
// fails here, before anything is created: widening the profile to fit an
// unrecognized install is never the fallback.
func identifyExecutable(m *manifest, binaryPath string, env []string) (*install, error) {
	resolved, err := exec.LookPath(binaryPath)
	if err != nil {
		return nil, &errBinaryNotFound{err: err}
	}
	if !filepath.IsAbs(resolved) {
		if resolved, err = filepath.Abs(resolved); err != nil {
			return nil, refuseErr(StageExecutable, err)
		}
	}
	exe, err := pinPath(resolved)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, &errBinaryNotFound{err: &exec.Error{Name: binaryPath, Err: os.ErrNotExist}}
		}
		return nil, refuseErr(StageExecutable, err)
	}
	if exe.mode != unix.S_IFREG {
		exe.close()
		return nil, refuse(StageExecutable, "%s resolves to %s, which is not a regular file", binaryPath, exe.canonical)
	}
	in := &install{exe: exe, path: exe.canonical}
	switch m.Executable.Kind {
	case "native":
		err = identifyNative(m, exe)
	case "npm-shim":
		err = identifyNPMShim(m, in, env)
	case "test":
		for _, dir := range m.testDirs {
			p, perr := pinPath(dir)
			if perr != nil {
				err = perr
				break
			}
			in.grants = append(in.grants, p)
		}
	default:
		err = fmt.Errorf("unknown executable kind %q", m.Executable.Kind)
	}
	if err != nil {
		in.close()
		return nil, refuseErr(StageExecutable, err)
	}
	return in, nil
}

// nativePlatforms lists the release platforms whose binaries may run here.
func nativePlatforms() []string {
	switch runtime.GOARCH {
	case "amd64":
		return []string{"linux-x64", "linux-x64-musl"}
	case "arm64":
		return []string{"linux-arm64", "linux-arm64-musl"}
	default:
		return nil
	}
}

type hashKey struct {
	id           objectID
	size         int64
	mtime, ctime int64
}

var (
	hashMu    sync.Mutex
	hashCache = map[hashKey]string{}
)

// identifyNative checks a native harness binary byte-for-byte against the
// release checksums of the pinned version. The hash is cached per (object,
// size, mtime, ctime), so a long-lived wrapper hashes each binary once.
func identifyNative(m *manifest, exe *pinned) error {
	var st unix.Stat_t
	if err := unix.Fstat(exe.fd, &st); err != nil {
		return err
	}
	var candidates []string
	for _, platform := range nativePlatforms() {
		if b, ok := m.Executable.Platforms[platform]; ok && b.Size == st.Size {
			candidates = append(candidates, platform)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("%s is not %s %s for %s/%s (unrecognized size %d); only the pinned version has a verified profile",
			exe.canonical, m.Name, m.HarnessVersion, runtime.GOOS, runtime.GOARCH, st.Size)
	}
	key := hashKey{id: exe.id, size: st.Size, mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano()}
	hashMu.Lock()
	sum, ok := hashCache[key]
	hashMu.Unlock()
	if !ok {
		f, err := os.Open("/proc/self/fd/" + strconv.Itoa(exe.fd))
		if err != nil {
			return fmt.Errorf("read %s: %w", exe.canonical, err)
		}
		head := make([]byte, 4)
		if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, []byte("\x7fELF")) {
			_ = f.Close()
			return fmt.Errorf("%s is not a native executable; layouts that start %s through an interpreter are refused", exe.canonical, m.Name)
		}
		h := sha256.New()
		h.Write(head)
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return fmt.Errorf("hash %s: %w", exe.canonical, err)
		}
		_ = f.Close()
		sum = hex.EncodeToString(h.Sum(nil))
		hashMu.Lock()
		hashCache[key] = sum
		hashMu.Unlock()
	}
	for _, platform := range candidates {
		if m.Executable.Platforms[platform].SHA256 == sum {
			return nil
		}
	}
	return fmt.Errorf("%s does not match the %s %s release checksums for %s", exe.canonical, m.Name, m.HarnessVersion, strings.Join(candidates, " or "))
}

// identifyNPMShim checks codex's npm layout: bin/codex.js in an
// @openai/codex package root at the pinned version, a matching vendored
// platform package inside it, and the node its shebang resolves to on the
// child's PATH. It grants the exact package root and the node executable,
// never a guessed ancestor or a whole global npm prefix.
func identifyNPMShim(m *manifest, in *install, env []string) error {
	exe := in.exe
	if filepath.Base(exe.canonical) != "codex.js" || filepath.Base(filepath.Dir(exe.canonical)) != "bin" {
		return fmt.Errorf("%s is not the %s package's bin/codex.js shim", exe.canonical, m.Executable.Package)
	}
	shebang, err := firstLine("/proc/self/fd/" + strconv.Itoa(exe.fd))
	if err != nil {
		return fmt.Errorf("read %s: %w", exe.canonical, err)
	}
	if shebang != "#!/usr/bin/env node" {
		return fmt.Errorf("%s starts with %q, not the #!/usr/bin/env node shim the profile knows", exe.canonical, shebang)
	}
	rootPath := filepath.Dir(filepath.Dir(exe.canonical))
	root, err := pinPath(rootPath)
	if err != nil {
		return err
	}
	in.grants = append(in.grants, root)
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := readJSONAt(root, "package.json", &pkg); err != nil {
		return err
	}
	if pkg.Name != m.Executable.Package || pkg.Version != m.HarnessVersion {
		return fmt.Errorf("%s holds %s %s; the profile is verified for %s %s only", rootPath, pkg.Name, pkg.Version, m.Executable.Package, m.HarnessVersion)
	}
	pp, ok := m.Executable.PlatformPackages[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no %s platform package for %s", m.Name, runtime.GOARCH)
	}
	vendorPath := filepath.Join(rootPath, "node_modules", pp.Alias, "vendor", pp.Triple)
	vendor, err := pinPath(vendorPath)
	if err != nil {
		return fmt.Errorf("the vendored %s binary is missing: %w", m.Name, err)
	}
	defer vendor.close()
	if !root.contains(vendor) {
		return fmt.Errorf("%s resolves outside the package root %s", vendorPath, rootPath)
	}
	var vpkg struct {
		Version    string `json:"version"`
		Target     string `json:"target"`
		Entrypoint string `json:"entrypoint"`
	}
	if err := readJSONAt(vendor, "codex-package.json", &vpkg); err != nil {
		return err
	}
	if vpkg.Version != m.HarnessVersion || vpkg.Target != pp.Triple {
		return fmt.Errorf("%s holds %s for %s; expected %s for %s", vendorPath, vpkg.Version, vpkg.Target, m.HarnessVersion, pp.Triple)
	}
	entry, err := pinPath(filepath.Join(vendorPath, filepath.Clean("/"+vpkg.Entrypoint)))
	if err != nil {
		return fmt.Errorf("the vendored %s entrypoint is missing: %w", m.Name, err)
	}
	defer entry.close()
	if entry.mode != unix.S_IFREG || !root.contains(entry) {
		return fmt.Errorf("the vendored %s entrypoint %s is not a regular file inside %s", m.Name, entry.canonical, rootPath)
	}
	node, err := lookPathIn(m.Executable.Interpreter, env)
	if err != nil {
		return fmt.Errorf("codex's node shim needs %q on the harness PATH: %w", m.Executable.Interpreter, err)
	}
	nodePin, err := pinPath(node)
	if err != nil {
		return err
	}
	if nodePin.mode != unix.S_IFREG {
		nodePin.close()
		return fmt.Errorf("%s is not a regular file", nodePin.canonical)
	}
	in.grants = append(in.grants, nodePin)
	return nil
}

// lookPathIn finds file on the PATH of env (the child's environment, which is
// what /usr/bin/env will search).
func lookPathIn(file string, env []string) (string, error) {
	path, _ := lookupEnv(env, "PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, file)
		if err := unix.Access(candidate, unix.X_OK); err == nil {
			if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
				return candidate, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.LimitReader(f, 256)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readJSONAt decodes name beneath the pinned directory dir.
func readJSONAt(dir *pinned, name string, v any) error {
	fd, err := unix.Openat(dir.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Join(dir.canonical, name), err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Join(dir.canonical, name), err)
	}
	return nil
}
