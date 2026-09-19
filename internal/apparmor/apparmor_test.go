package apparmor

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestValidateRoot(t *testing.T) {
	for _, ok := range []string{"/srv/work", "/home/u/.local/state/harness-wrapper", "/a/b-c_d.e+f~g:h=i%j"} {
		if err := ValidateRoot(ok); err != nil {
			t.Errorf("ValidateRoot(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "relative", "/", "/srv/work/", "/srv/../etc", "/srv//work",
		"/srv/*", "/srv/**", "/srv/wo?k", "/srv/[ab]", "/srv/{a,b}", "/srv/^x",
		"/srv/@{HOME}", `/srv/"x`, `/srv/x\y`, "/srv/a b", "/srv/a\tb", "/srv/a\nrw,", "/srv/a,b", "/srv/#x",
	} {
		if err := ValidateRoot(bad); !errors.Is(err, ErrInvalidRoot) {
			t.Errorf("ValidateRoot(%q) = %v, want ErrInvalidRoot", bad, err)
		}
	}
}

func TestProfileRoundTrip(t *testing.T) {
	src, err := Profile([]string{"/srv/work", "/home/u/state", "/srv/work"})
	if err != nil {
		t.Fatal(err)
	}
	layer, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	roots := layer.Roots
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/home/u/state", "/srv/work"}; !slices.Equal(roots, want) {
		t.Fatalf("roots = %v, want %v (sorted, deduplicated)", roots, want)
	}
	for _, want := range []string{
		"abi <abi/3.0>,",
		"profile " + NameFor(roots) + " flags=(attach_disconnected,mediate_deleted) {",
		"  /** rmixlk,",
		"  /dev/null rw,",
		"  /dev/tty rw,",
		"  /dev/pts/[0-9]* rw,",
		`  "/home/u/state/{,**}" rw,`,
		`  "/srv/work/{,**}" rw,`,
	} {
		if !strings.Contains(src, want+"\n") {
			t.Errorf("profile lacks line %q:\n%s", want, src)
		}
	}
	// The only write rules are the devices and the roots: a stray "w"
	// anywhere else would reopen every socket beneath it.
	var writes []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "abi ") || !strings.HasSuffix(line, ",") {
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(line, ","))
		if len(fields) == 2 && strings.ContainsAny(fields[1], "wa") {
			writes = append(writes, fields[0])
		}
	}
	want := []string{"/dev/null", "/dev/tty", "/dev/pts/[0-9]*", `"/home/u/state/{,**}"`, `"/srv/work/{,**}"`}
	if !slices.Equal(writes, want) {
		t.Errorf("write rules = %v, want exactly %v", writes, want)
	}
}

// The loaded profile's name is bound to the policy: the same roots name the
// same profile, and any change to them — narrowing included — names another,
// so a source regenerated without a reload names a profile that is not loaded.
func TestProfileNameBindsPolicy(t *testing.T) {
	wide, err := Profile([]string{"/srv/work"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Profile([]string{"/srv/work", "/srv/work"})
	narrow, _ := Profile([]string{"/srv/work/project"})
	lw, err := Parse([]byte(wide))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Parse([]byte(narrow))
	if err != nil {
		t.Fatal(err)
	}
	la, _ := Parse([]byte(again))
	if lw.Profile == ln.Profile {
		t.Fatalf("narrowing the roots kept the profile name %s", lw.Profile)
	}
	if la.Profile != lw.Profile {
		t.Fatalf("the same roots named %s and %s", la.Profile, lw.Profile)
	}
	if !strings.HasPrefix(lw.Profile, ProfileName+"-") || len(lw.Profile) != len(ProfileName)+1+digestLen {
		t.Fatalf("profile name %q is not %s-<%d hex digits>", lw.Profile, ProfileName, digestLen)
	}
	if !strings.Contains(wide, "profile "+lw.Profile+" flags=") {
		t.Fatalf("the source does not declare the profile it names:\n%s", wide)
	}
}

// A source that is not exactly what Profile generates for its header's roots
// describes no profile the wrapper can vouch for: its body may say anything.
func TestParseRefusesEditedSource(t *testing.T) {
	src, _ := Profile([]string{"/srv/work"})
	for name, edited := range map[string]string{
		"widened body":  strings.Replace(src, `"/srv/work/{,**}" rw,`, `"/srv/{,**}" rw,`, 1),
		"extra rule":    strings.Replace(src, "  /** rmixlk,", "  /** rmixlk,\n  /run/** w,", 1),
		"renamed":       strings.Replace(src, NameFor([]string{"/srv/work"}), ProfileName, 1),
		"trailing text": src + "# note\n",
	} {
		if _, err := Parse([]byte(edited)); err == nil {
			t.Errorf("%s: Parse accepted an edited source", name)
		}
	}
}

func TestProfileRefuses(t *testing.T) {
	if _, err := Profile(nil); !errors.Is(err, ErrInvalidRoot) {
		t.Errorf("Profile(nil) = %v, want ErrInvalidRoot", err)
	}
	if _, err := Profile([]string{"/srv/ok", "/srv/*"}); !errors.Is(err, ErrInvalidRoot) {
		t.Errorf("a pattern root: %v, want ErrInvalidRoot", err)
	}
}

func TestParseRootsRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"no header":        "abi <abi/3.0>,\nprofile x {}\n",
		"future version":   "# " + ProfileName + " v2 roots: [\"/srv\"]\n",
		"bad json":         headerPrefix + "[\"/srv\"\n",
		"no roots":         headerPrefix + "[]\n",
		"root is slash":    headerPrefix + "[\"/\"]\n",
		"root is a glob":   headerPrefix + "[\"/srv/**\"]\n",
		"header not first": "\n" + headerPrefix + "[\"/srv\"]\n",
	} {
		if _, err := ParseRoots([]byte(src)); err == nil {
			t.Errorf("%s: ParseRoots accepted %q", name, src)
		}
	}
}

func TestCovers(t *testing.T) {
	roots := []string{"/srv/work", "/home/u/state"}
	for path, want := range map[string]bool{
		"/srv/work":              true,
		"/srv/work/repo":         true,
		"/home/u/state/x/y":      true,
		"/srv/workshop":          false,
		"/srv":                   false,
		"/home/u":                false,
		"/run/user/1000/bus":     false,
		"/home/u/state-other/sk": false,
	} {
		if got := Covers(roots, path); got != want {
			t.Errorf("Covers(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLabelHas(t *testing.T) {
	for label, want := range map[string]bool{
		ProfileName + "//&unconfined (enforce)\n":          true,
		ProfileName + " (enforce)":                         true,
		"outer (complain)//&" + ProfileName + " (enforce)": true,
		ProfileName + " (enforce)//&outer (complain)":      true,
		ProfileName + "//&unconfined (complain)":           false,
		ProfileName + " (complain)//&outer (enforce)":      false,
		"unconfined":                                 false,
		"other//&unconfined (enforce)":               false,
		ProfileName + "-evil//&unconfined (enforce)": false,
		ProfileName + "//&unconfined (mixed)":        false,
		ProfileName:                                  false,
		"":                                           false,
	} {
		if got := labelHas(label, ProfileName); got != want {
			t.Errorf("labelHas(%q) = %v, want %v", label, got, want)
		}
	}
}
