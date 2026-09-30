package saveload

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A run may hand what it saved to a run on another machine: the truest form
// of "the source is gone".
const (
	// envExport names a directory a run leaves its archives in, with what a
	// load elsewhere needs to check them.
	envExport = "HW_SAVELOAD_EXPORT"
	// envImport names such a directory, copied from the machine that saved
	// it: the run saves nothing, and loads that.
	envImport = "HW_SAVELOAD_IMPORT"
	// envRevision names the tree a test binary was built from, for a run on
	// a machine without the tree.
	envRevision = "HW_SAVELOAD_REVISION"
)

// handoffName is the file a saving run describes its save in, beside the
// archives.
const handoffName = "saved.json"

// handoff is what a load on another machine needs to know of a save: what the
// conversation should carry, and the Session's native id and state. None of it
// is the conversation itself, which only the archives hold.
type handoff struct {
	Harness  string        `json:"harness"`
	Mode     mode          `json:"mode"`
	Platform string        `json:"platform"`
	Host     string        `json:"host"`
	Versions versionsShown `json:"versions"`
	// Work is the saving run's work directory, as given and resolved: the
	// loading run's evidence names it $SOURCE.
	Work       []string             `json:"work"`
	Nonces     nonces               `json:"nonces"`
	SessionID  string               `json:"session_id"`
	Checkpoint *contract.Checkpoint `json:"checkpoint"`
	Aux        map[string]string    `json:"aux"`
	Variants   []variant            `json:"variants"`
	Source     environmentShown     `json:"source"`
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return name
}

// export leaves a save where envExport says: every variant's archive, and the
// hand-off.
func (p *probe) export(dir string, sv saved) {
	t := p.t
	dir = filepath.Join(dir, p.h.Name()+"-"+string(p.mode))
	fatalIf(t, os.RemoveAll(dir), "export")
	fatalIf(t, os.MkdirAll(dir, 0o700), "export")
	for _, v := range sv.variants {
		fatalIf(t, copyFile(sv.archives[v.Name], filepath.Join(dir, v.Name+".tar")), "exporting the %s archive", v.Name)
	}
	h := handoff{
		Harness: p.h.Name(), Mode: p.mode, Platform: runtime.GOOS + "/" + runtime.GOARCH, Host: hostname(),
		Versions: p.rep.Versions, Work: []string{p.work, canonical(p.work)},
		Nonces: p.nonce, SessionID: sv.sessionID, Checkpoint: sv.cp, Aux: sv.aux, Variants: sv.variants, Source: p.rep.Source,
	}
	data, err := json.MarshalIndent(h, "", "  ")
	fatalIf(t, err, "export")
	fatalIf(t, os.WriteFile(filepath.Join(dir, handoffName), append(data, '\n'), 0o600), "export")
	t.Logf("exported %d archives and %s to %s", len(sv.variants), handoffName, dir)
}

// imported takes a save made on another machine in place of one of this
// run's: the same nonces and script, the archives as they were copied here.
// The harness must be the one that saved it, at the same version.
func (p *probe) imported(dir string) saved {
	t := p.t
	dir = filepath.Join(dir, p.h.Name()+"-"+string(p.mode))
	data, err := os.ReadFile(filepath.Join(dir, handoffName)) //nolint:gosec // the directory the caller named
	fatalIf(t, err, "import")
	var h handoff
	fatalIf(t, json.Unmarshal(data, &h), "import: %s", handoffName)
	if h.Harness != p.h.Name() || h.Mode != p.mode || h.Versions.Binary != p.rep.Versions.Binary {
		t.Fatalf("import: the save is of %s %q in %s mode; this run is %s %q in %s mode", h.Harness, h.Versions.Binary, h.Mode, p.h.Name(), p.rep.Versions.Binary, p.mode)
	}
	p.nonce, p.script = h.Nonces, p.h.Script(p.mode, h.Nonces)
	p.rep.elsewhere = h.Work
	sv := saved{sessionID: h.SessionID, cp: h.Checkpoint, aux: h.Aux, variants: h.Variants, archives: map[string]string{}}
	for _, v := range h.Variants {
		sv.archives[v.Name] = filepath.Join(dir, v.Name+".tar")
		m, err := readManifest(sv.archives[v.Name])
		fatalIf(t, err, "import: the %s archive", v.Name)
		info, err := os.Stat(sv.archives[v.Name])
		fatalIf(t, err, "import: the %s archive", v.Name)
		p.rep.Archives = append(p.rep.Archives, archiveReport{Variant: v.Name, Note: v.Note, Bytes: info.Size(), Entries: m.Entries})
	}
	// The source's own account of itself, under the name its paths take here.
	raw, _ := json.Marshal(h.Source)
	_ = json.Unmarshal([]byte(strings.NewReplacer("$WORK", "$SOURCE", "-WORK", "-SOURCE").Replace(string(raw))), &p.rep.Source)
	p.check("source.elsewhere", "the archive was saved on another machine, which this one cannot reach into", true,
		h.Host != hostname(), "saved on %s (%s, %s); loaded on %s (%s/%s)", h.Host, h.Platform, h.Versions.Binary, hostname(), runtime.GOOS, runtime.GOARCH)
	return sv
}

func copyFile(from, to string) (err error) {
	in, err := os.Open(from) //nolint:gosec // a path of the probe's own work directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the directory the caller named
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}
