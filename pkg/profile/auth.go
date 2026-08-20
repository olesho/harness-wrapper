package profile

// claudePerRootKeychainMinVersion is the first claude version that keys macOS
// keychain credentials PER CONFIG ROOT.
//
// Measured 2026-08-19 on claude 2.1.234/235: the slot OUTRANKS the credentials
// file — claude migrates .credentials.json into the slot and then DELETES the
// file — so a stale slot means "Login expired" no matter how fresh the file is.
// That cost two failed runs before diagnosis, which is why the rule lives here
// as data instead of in whoever remembers it.
const claudePerRootKeychainMinVersion = "2.1.234"

// claudeBaseKeychainService is the operator's own (unsuffixed) keychain item,
// i.e. the one a non-profiled claude uses.
const claudeBaseKeychainService = "Claude Code-credentials"

// AuthSlot describes WHERE a harness looks for credentials for one config root.
// It is data only: this package never calls security(1), never reads a
// keychain, and never acquires or refreshes a token. The caller does all of
// that with the names below.
type AuthSlot struct {
	// Service is the full keychain service name for this config root, e.g.
	// "Claude Code-credentials-7629796b".
	Service string

	// Account is deliberately EMPTY. The keychain account is the operator's
	// macOS login name — host state, not harness knowledge — so the caller
	// supplies it. Nothing in this package may read $USER to fill it in.
	Account string

	// BaseService is the unsuffixed service name: the operator's own item,
	// useful when copying credentials into a per-root slot.
	BaseService string

	// Suffix is the first 8 hex characters of sha256(configDir), the part that
	// makes the slot per-root. It is exported so a shell implementation can be
	// diffed against this one:
	//   printf '%s' "$dir" | shasum -a 256 | cut -c1-8
	Suffix string

	// OutranksFile is true when the slot wins over CredentialsFile — i.e. a
	// stale slot logs the agent out even with a valid file on disk.
	OutranksFile bool

	// CredentialsFile is the file-based credential path, relative to the
	// config root.
	CredentialsFile string
}

// KeychainSlot returns the per-config-root credential slot rule in force for
// this harness at this version.
//
// ok=false means "no per-root slot applies" — file-based auth only. That is the
// answer for claude before 2.1.234, for codex at any version, for an unknown
// harness, and, deliberately, for a version string with no parseable X.Y.Z: the
// package FAILS CLOSED to file-based auth rather than guessing a slot, because
// a wrong slot silently logs the agent out while no slot at worst reproduces
// the pre-2.1.234 behaviour.
//
// configDir must be the LITERAL value the caller injects as CLAUDE_CONFIG_DIR —
// absolute, no trailing slash, no symlink resolution — hashed with no trailing
// newline. A normalised variant hashes to a different, wrong slot, so callers
// must not normalise it here or there. Worked vector: "/tmp/p/claude" yields
// Suffix "7629796b" and Service "Claude Code-credentials-7629796b".
func KeychainSlot(h Harness, configDir, harnessVersion string) (AuthSlot, bool) {
	if h != Claude {
		return AuthSlot{}, false
	}
	atLeast, err := VersionAtLeast(harnessVersion, claudePerRootKeychainMinVersion)
	if err != nil || !atLeast {
		return AuthSlot{}, false
	}
	suffix := shortSum(configDir)
	return AuthSlot{
		Service:         claudeBaseKeychainService + "-" + suffix,
		Account:         "",
		BaseService:     claudeBaseKeychainService,
		Suffix:          suffix,
		OutranksFile:    true,
		CredentialsFile: ".credentials.json",
	}, true
}
