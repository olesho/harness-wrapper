// Package sessionid mints and checks the session ids a harness accepts for a
// fresh session at launch. Claude Code and pi both take a UUID
// (--session-id <uuid>), and both name the session's transcript after it.
package sessionid

import (
	"crypto/rand"
	"fmt"
	"regexp"
)

// uuidRE matches a UUID in its canonical 8-4-4-4-12 hexadecimal form, in
// either case. It is the shape Claude Code validates --session-id against.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NewUUID returns a random UUID (version 4, RFC 9562 variant) in canonical
// lower-case form.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail; it panics instead
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// IsUUID reports whether s is a UUID in canonical form.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }
