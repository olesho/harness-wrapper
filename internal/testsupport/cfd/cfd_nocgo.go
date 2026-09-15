//go:build !(linux && cgo)

// Package cfd opens file descriptors from C for the containment tests; in a
// build without cgo (or off Linux) there is no C to open them from.
package cfd

// Available reports whether this build can open descriptors from C.
const Available = false

// OpenNonCloexec returns -1: no C here.
func OpenNonCloexec(string) int { return -1 }

// ListenNonCloexec returns -1: no C here.
func ListenNonCloexec() int { return -1 }

// Close does nothing.
func Close(int) {}

// StartOpeners starts nothing.
func StartOpeners(string, int) int { return 0 }

// StopOpeners returns 0.
func StopOpeners() int64 { return 0 }
