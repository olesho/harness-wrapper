package saveload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// report is a run's evidence: what ran, what was saved, and every check's
// result. Paths in it are relative to the run's work directory ($WORK), and
// it holds no credential: a mock run's are placeholders, a live run's never
// leave the file they were staged in.
type report struct {
	p *probe
	// elsewhere is the work directory of the run that saved what this one
	// loads, when that was another machine's: $SOURCE in the evidence.
	elsewhere []string

	Harness  string        `json:"harness"`
	Mode     mode          `json:"mode"`
	When     string        `json:"when"`
	Platform string        `json:"platform"`
	Versions versionsShown `json:"versions"`
	// Launches are the harness's command lines while its Sessions ran, as ps
	// showed them.
	Launches    []launchShown    `json:"launches"`
	Source      environmentShown `json:"source"`
	Restored    environmentShown `json:"restored"`
	Relocations []relocation     `json:"relocations"`
	// Inventory is every file the source held when it was saved.
	Inventory []inventoryRow `json:"inventory"`
	// Skipped are the source's entries that are not regular files: never
	// saved.
	Skipped []string `json:"skipped,omitempty"`
	// Outside is what the source\'s harness left outside the five roots:
	// never saved either.
	Outside  []entry         `json:"outside_the_roots,omitempty"`
	Archives []archiveReport `json:"archives"`
	Checks   []check         `json:"checks"`
	Variants []variantReport `json:"variants,omitempty"`
	Requests []requestShown  `json:"requests,omitempty"`
	Records  []recordShown   `json:"records,omitempty"`
}

type versionsShown struct {
	Binary           string `json:"binary"`
	BinarySHA256     string `json:"binary_sha256"`
	Pinned           string `json:"pinned"`
	Adapter          string `json:"adapter"`
	Revision         string `json:"revision,omitempty"`
	Contract         string `json:"contract"`
	CheckpointFormat int    `json:"checkpoint_format"`
}

type launchShown struct {
	When    string `json:"when"`
	Command string `json:"command"`
}

type environmentShown struct {
	Layout contract.Layout `json:"layout"`
	// Workspace is the workspace with its symlinks resolved.
	Workspace   string `json:"workspace_resolved"`
	HarnessRoot string `json:"harness_root"`
	SessionID   string `json:"session_id"`
	// Record is the Session's native record.
	Record     string `json:"record"`
	Credential string `json:"credential,omitempty"`
}

type inventoryRow struct {
	// Path is a file, or a directory's files together (…/**).
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	// Class: rendered by Provision, history (beneath a history root), secret
	// (beneath a secret path), or other — the harness's own, which the
	// adapter declares nothing about.
	Class string `json:"class"`
	// In names the variants that save it.
	In []string `json:"in,omitempty"`
}

type archiveReport struct {
	Variant string  `json:"variant"`
	Note    string  `json:"note"`
	Bytes   int64   `json:"bytes"`
	Entries []entry `json:"entries"`
}

type check struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Required checks are the gate's; the others record what was found.
	Required bool `json:"required"`
	// Result is pass or fail for a required check, and info otherwise.
	Result string `json:"result"`
	Detail string `json:"detail"`
}

type variantReport struct {
	Variant      string            `json:"variant"`
	Note         string            `json:"note"`
	Files        int               `json:"files"`
	Conversation bool              `json:"conversation_continued"`
	SameSession  bool              `json:"same_session_id"`
	Aux          map[string]string `json:"native_state,omitempty"`
	Detail       string            `json:"detail"`
}

type requestShown struct {
	Name string `json:"name"`
	// N is the request's number among all the mock answered.
	N     int    `json:"n"`
	Items []item `json:"conversation"`
}

type recordShown struct {
	Name         string        `json:"name"`
	Observations []observation `json:"observations"`
}

type observation struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Origin string `json:"origin"`
	Input  string `json:"input_id,omitempty"`
	Time   string `json:"time"`
	Text   string `json:"text,omitempty"`
}

func newReport(p *probe, version string) *report {
	r := &report{
		p: p, Harness: p.h.Name(), Mode: p.mode,
		When:     time.Now().UTC().Format(time.RFC3339),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Versions: versionsShown{
			Binary: version, Pinned: p.desc.Harness.Version, Adapter: p.desc.Harness.Adapter,
			Contract: p.desc.Contract, CheckpointFormat: p.desc.CheckpointFormat,
		},
	}
	if sum, _, err := hashFile(p.bin, -1); err == nil {
		r.Versions.BinarySHA256 = sum
	}
	// The tree the probe was built from: named by the caller where the test
	// binary runs away from it.
	r.Versions.Revision = os.Getenv(envRevision)
	if out, err := exec.Command("git", "describe", "--always", "--dirty", "--tags").Output(); err == nil && r.Versions.Revision == "" {
		r.Versions.Revision = strings.TrimSpace(string(out))
	}
	return r
}

// secretLike matches what a real credential looks like; nothing of the kind
// belongs in the evidence.
var secretLike = regexp.MustCompile(`sk-[A-Za-z0-9_-]{24,}`)

// clean makes a string fit for the evidence: the run's directories by name,
// not by where this machine put them, and nothing shaped like a credential.
func (r *report) clean(s string) string {
	for _, dir := range r.elsewhere {
		s = strings.ReplaceAll(s, tclaude.EncodedCWD(dir), "-SOURCE")
		s = strings.ReplaceAll(s, dir, "$SOURCE")
	}
	for _, dir := range []string{canonical(r.p.work), r.p.work} {
		// The directory may also be there as claude names a workspace's
		// transcripts: every character that is no letter or digit a dash.
		s = strings.ReplaceAll(s, tclaude.EncodedCWD(dir), "-WORK")
		s = strings.ReplaceAll(s, dir, "$WORK")
		// A path cut short — a reply that quotes the start of a tool's
		// output — is the longest prefix of the directory the string holds.
		for n := len(dir) - 1; n >= 20; n-- {
			if strings.Contains(s, dir[:n]) {
				s = strings.ReplaceAll(s, dir[:n], "$WORK…")
				break
			}
		}
	}
	for _, bin := range []string{canonical(r.p.bin), r.p.bin} {
		s = strings.ReplaceAll(s, bin, "$BIN")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return secretLike.ReplaceAllString(s, "sk-…")
}

func (r *report) cleanErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return r.clean(err.Error())
}

func (r *report) environment(e *environment, sessionID, record string) environmentShown {
	l := e.layout
	shown := environmentShown{
		Layout: contract.Layout{
			Home: r.clean(l.Home), Config: r.clean(l.Config), Workspace: r.clean(l.Workspace),
			Secrets: r.clean(l.Secrets), Scratch: r.clean(l.Scratch),
		},
		Workspace: r.clean(canonical(l.Workspace)), HarnessRoot: r.clean(e.root),
		SessionID: sessionID, Record: r.clean(record),
	}
	if e.cred != nil {
		shown.Credential = e.cred.Kind + " in " + r.clean(e.cred.File)
	}
	return shown
}

// launched keeps the command line of the harness running in e.
func (r *report) launched(when string, e *environment) {
	for _, line := range processes(filepath.Join(e.root, "bin")) {
		_, command, _ := strings.Cut(line, " ")
		r.Launches = append(r.Launches, launchShown{When: when, Command: r.clean(strings.TrimSpace(command))})
	}
}

// check records a required check's result.
func (p *probe) check(id, title string, required, ok bool, format string, args ...any) {
	result := "pass"
	if !ok {
		result = "fail"
	}
	c := check{ID: id, Title: title, Required: required, Result: result, Detail: p.rep.clean(fmt.Sprintf(format, args...))}
	p.rep.Checks = append(p.rep.Checks, c)
	p.t.Logf("%s %s — %s", strings.ToUpper(result), id, c.Detail)
}

// info records something found that the gate does not decide on.
func (p *probe) info(id, title, format string, args ...any) {
	c := check{ID: id, Title: title, Result: "info", Detail: p.rep.clean(fmt.Sprintf(format, args...))}
	p.rep.Checks = append(p.rep.Checks, c)
	p.t.Logf("INFO %s — %s", id, c.Detail)
}

func (r *report) failed() bool {
	for _, c := range r.Checks {
		if c.Required && c.Result != "pass" {
			return true
		}
	}
	return false
}

// short cuts a text for the evidence.
func short(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// request keeps a model request's conversation: each item's role and kind,
// and the start of its text. The system prompt and the tools are left out.
func (r *report) request(name string, n int, conv []item) {
	shown := requestShown{Name: name, N: n}
	for _, it := range conv {
		limit := 200
		if synthetic(it.Text) {
			limit = 60
		}
		it.Text = short(r.clean(it.Text), limit)
		shown.Items = append(shown.Items, it)
	}
	r.Requests = append(r.Requests, shown)
}

// record keeps what a record reading or a Session delivered: each
// observation's id and kind, and the start of its text.
func (r *report) record(name string, seen []contract.Observation) {
	shown := recordShown{Name: name, Observations: []observation{}}
	for _, o := range seen {
		var text struct {
			Text   string          `json:"text"`
			Output string          `json:"output"`
			Name   string          `json:"name"`
			Input  json.RawMessage `json:"input"`
		}
		_ = o.Decode(&text)
		t := text.Text + text.Output
		if text.Name != "" {
			t = text.Name + " " + string(text.Input)
		}
		shown.Observations = append(shown.Observations, observation{
			ID: o.ID, Kind: string(o.Kind), Origin: string(o.Origin), Input: o.InputID,
			Time: o.Time.UTC().Format(time.RFC3339), Text: short(r.clean(t), 120),
		})
	}
	r.Records = append(r.Records, shown)
}

// beneath reports whether root/path is one of paths, or beneath one.
func beneath(paths []contract.RootPath, root contract.Root, path string) bool {
	for _, p := range paths {
		if p.Root == root && (path == p.Path || strings.HasPrefix(path, p.Path+"/")) {
			return true
		}
	}
	return false
}

// inventory lists what the source held when it was saved: the files a variant
// saves and the ones beneath a history root or a secret path each on a line,
// the rest together by their top directory.
func (r *report) inventory(src *environment, files []entry, variants []variant) {
	rendered := map[contract.RootPath]bool{}
	for _, f := range src.result.Files {
		rendered[contract.RootPath{Root: f.Root, Path: f.Path}] = true
	}
	rows := map[string]*inventoryRow{}
	for _, f := range files {
		class := "other"
		switch {
		case beneath(src.result.SecretPaths, f.Root, f.Path):
			class = "secret"
		case beneath(src.result.HistoryRoots, f.Root, f.Path):
			class = "history"
		case rendered[contract.RootPath{Root: f.Root, Path: f.Path}]:
			class = "rendered"
		}
		var in []string
		for _, v := range variants {
			if v.holds(f.Root, f.Path) {
				in = append(in, v.Name)
			}
		}
		key := f.name()
		if top, _, nested := strings.Cut(f.Path, "/"); nested && class == "other" && len(in) == 0 {
			key = string(f.Root) + "/" + top + "/**"
		}
		row := rows[key]
		if row == nil {
			row = &inventoryRow{Path: key, Class: class, In: in}
			rows[key] = row
		}
		row.Files++
		row.Bytes += f.Size
	}
	for _, row := range rows {
		r.Inventory = append(r.Inventory, *row)
	}
	sort.Slice(r.Inventory, func(i, j int) bool { return r.Inventory[i].Path < r.Inventory[j].Path })
}

func auxString(aux map[string]string) string {
	if aux == nil {
		return "none"
	}
	keys := make([]string, 0, len(aux))
	for k := range aux {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + fmt.Sprintf("%q", aux[k])
	}
	return strings.Join(parts, ", ")
}

func sameAux(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// table is the run's result table, in Markdown.
func (r *report) table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s: %s (pinned %s), %s, %s\n\n", r.Harness, r.Mode, r.Versions.Binary, r.Versions.Pinned, r.Versions.Adapter, r.Platform)
	b.WriteString("| Check | Result | What was seen |\n|---|---|---|\n")
	cell := strings.NewReplacer("|", "\\|", "\n", " ")
	for _, c := range r.Checks {
		result := c.Result
		if c.Required {
			result = "**" + result + "**"
		}
		fmt.Fprintf(&b, "| `%s` %s | %s | %s |\n", c.ID, cell.Replace(c.Title), result, cell.Replace(c.Detail))
	}
	if len(r.Variants) > 0 {
		b.WriteString("\n| Saved state | Files | Conversation continued | Same Session id | Native state read back |\n|---|---|---|---|---|\n")
		for _, v := range r.Variants {
			fmt.Fprintf(&b, "| `%s`: %s | %d | %v | %v | %s |\n", v.Variant, cell.Replace(v.Note), v.Files, v.Conversation, v.SameSession, cell.Replace(auxString(v.Aux)))
		}
	}
	return b.String()
}

// write writes the evidence, when a directory for it was named:
// <harness>-<mode>.json, all of it, and .md, its tables.
func (r *report) write() {
	dir := os.Getenv(envEvidence)
	if dir == "" {
		return
	}
	t := r.p.t
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // evidence meant to be read
		t.Errorf("evidence: %v", err)
		return
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Errorf("evidence: %v", err)
		return
	}
	var md strings.Builder
	md.WriteString(r.table())
	md.WriteString("\n| Source file | Files | Bytes | Class | Saved by |\n|---|---|---|---|---|\n")
	for _, row := range r.Inventory {
		fmt.Fprintf(&md, "| `%s` | %d | %d | %s | %s |\n", r.clean(row.Path), row.Files, row.Bytes, row.Class, strings.Join(row.In, ", "))
	}
	// Every path, wherever it sits in the evidence, by the run's names.
	data = []byte(r.clean(string(data)))
	base := filepath.Join(dir, r.Harness+"-"+string(r.Mode))
	for name, content := range map[string][]byte{base + ".json": append(data, '\n'), base + ".md": []byte(r.clean(md.String()))} {
		if err := os.WriteFile(name, content, 0o644); err != nil { //nolint:gosec // evidence meant to be read
			t.Errorf("evidence: %v", err)
		}
	}
	sum := sha256.Sum256(data)
	t.Logf("evidence written to %s.{json,md} (sha256 %s)", base, hex.EncodeToString(sum[:8]))
}
