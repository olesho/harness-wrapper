module github.com/olesho/harness-wrapper

go 1.25.0

require (
	github.com/creack/pty v1.1.24
	github.com/hinshun/vt10x v0.0.0-20220301184237-5011da428d02
	golang.org/x/term v0.42.0
)

require golang.org/x/sys v0.43.0 // indirect

// v0.7.8 was tagged by mistake on 2026-09-10 from a branch cut at v0.7.7. It
// carries one process-group change (PUPPET-610) and none of v0.8.0-v0.8.4, so
// selecting it is a downgrade; the change itself ships in v0.8.5.
retract v0.7.8 // tagged by mistake from a v0.7.7 branch; a downgrade from v0.8.x
