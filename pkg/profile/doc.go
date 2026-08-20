// Package profile owns the agent-profile contract: the `.manifest.json`
// launch-verification record, the fingerprint algorithm that record is built
// on, the per-harness config-root layout (what a provisioner copies versus what
// the harness owns at runtime), and the version-gated credential-slot rules —
// as data.
//
// # Why this package exists
//
// The contract was implemented twice, independently: once in Go inside loomcli's
// supervisor (which refuses to boot an agent whose profile does not verify) and
// once in Python-inside-bash in the PUPPET workspace's provision script (which
// writes the profile). On 2026-08-19 the two diverged on day one — the script
// folded `harness_version` into the sha256 while the supervisor hashed only
// path + NUL + bytes — producing a false boot refusal on the first profiled run.
// A contract implemented twice is a contract that drifts, so it lives here once
// and both sides become callers.
//
// # Purity boundary
//
// This package is a pure translator: `fs.FS` (or plain strings) in, values out.
// It performs no keychain access, executes no process, queries no supervisor,
// resolves no environment variable, and copies nothing of its own. BuildPlan
// returns a plan; the caller does the copying. KeychainSlot returns the service
// name and precedence rule in force; the caller talks to the keychain. That
// boundary is what makes the package safe to depend on from both a provisioner
// that writes and a supervisor that refuses to boot, and it is asserted
// mechanically by a test (see purity_test.go).
//
// Acquisition and lifecycle stay with the caller. In particular, "the manifest
// file is absent" is deliberately not an error of this package: the caller does
// the read and owns the never-provisioned distinction.
//
// # The manifest
//
// A provisioned config root carries ManifestName at its top level:
//
//	{
//	 "files": ["CLAUDE.md", "settings.json", "skills/x.md"],
//	 "fingerprint": "27fa9696...",
//	 "harness_version": "2.1.235 (Claude Code)"
//	}
//
// Files is an ALLOWLIST, and that is load-bearing. Files present in the config
// root but absent from the list are never opened, because the harness mutates
// .credentials.json, .claude.json, sessions/ and history/ at runtime by design;
// hashing them would make every profile fail verification within minutes.
//
// # The fingerprint algorithm, byte for byte
//
// Let files be the manifest's Files list, in the order listed (Validate
// requires that order to be sorted and duplicate-free, so listed order and
// sorted order are the same thing, and a builder and a verifier cannot
// disagree about it). Then:
//
//	h := sha256.New()
//	for each rel in files:
//	    h.Write(UTF-8 bytes of rel)   // slash-separated, relative, path.Clean-stable
//	    h.Write([]byte{0x00})
//	    h.Write(the file's bytes, verbatim)
//	fingerprint := lowercase hex of h.Sum(nil)
//
// Nothing else is an input. In particular harness_version is NOT hashed: it is
// a separate manifest field compared on its own. Folding it in would conflate
// two distinct repairs — "re-provision this profile" and "bless this harness
// upgrade" — into one indistinguishable mismatch, and callers depend on telling
// them apart. An empty file list is legal and hashes to the sha256 of the empty
// string. A file listed but missing on disk is an error, never a skip.
//
// A reimplementation in another language agrees with this one if it produces
// 27fa969635caf3dc34026424a3bfac5b066d7b20c8e96dcc2cfc991c0e4bd99b for the tree
// {"a.txt": "A", "b/c.txt": "C"} listed as ["a.txt", "b/c.txt"].
//
// # Layout
//
// Every file under a source profile tree is provisioned (copied and manifested)
// EXCEPT the declared exceptions: seed files, which are copied only if absent at
// the destination and are never manifested or hashed because the harness
// rewrites them; excluded directories, which are never walked; and junk
// (.DS_Store). See Harness.SeedFiles and Harness.ExcludedDirs.
//
// # Version drift
//
// Drift detection is exact string equality of the trimmed harness_version — the
// first line of `<binary> --version` as the caller observed it. There is no
// semver leniency: "2.1.235 (Claude Code)" is compared whole. Semver parsing
// exists in this package for exactly one purpose, gating the auth rules below.
//
// Lineage: PUPPET-96 (profile/manifest contract), PUPPET-122 (this package).
package profile
