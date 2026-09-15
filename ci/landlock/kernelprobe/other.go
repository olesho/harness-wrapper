//go:build !linux

// Command kernelprobe is Linux-only; elsewhere it reports that and fails.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kernelprobe: Landlock is Linux-only")
	os.Exit(1)
}
