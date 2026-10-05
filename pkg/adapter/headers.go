package adapter

import (
	"fmt"
	"sort"
	"strings"
)

// HeadersScript is a shell script that prints the headers of MCP server name
// as a harness's headers helper answers them, a JSON object: each header's
// value read from its file in files, its newlines dropped and its
// backslashes, quotes and tabs escaped. It holds the files' paths and no
// value, and a file it cannot read fails it. A profile writes it as a file
// of its own under config, which the harness runs with /bin/sh each time it
// connects (no provisioned file is executable): a connector's headers_file.
func HeadersScript(name string, files map[string]string) string {
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n# The headers of MCP server %s, read from their files each time the\n# harness connects: no value is in its configuration or environment.\nset -eu\n", name)
	b.WriteString("v() { tr -d '\\n' <\"$1\" | sed -e 's/\\\\/\\\\\\\\/g' -e 's/\"/\\\\\"/g' -e 's/\t/\\\\t/g'; }\n")
	for _, k := range names {
		fmt.Fprintf(&b, "[ -r %s ] || { echo %s >&2; exit 1; }\n", ShellQuote(files[k]), ShellQuote("cannot read the file of header "+k))
	}
	b.WriteString("printf '{'\n")
	for i, k := range names {
		sep := ","
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&b, "printf '%s\"%%s\":\"%%s\"' %s \"$(v %s)\"\n", sep, ShellQuote(k), ShellQuote(files[k]))
	}
	b.WriteString("printf '}\\n'\n")
	return b.String()
}

// ShellQuote is s as one shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
