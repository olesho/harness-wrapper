package adapter

import (
	"fmt"
	"sort"
	"strings"
)

// HeadersScript is a shell script that prints the headers of MCP server name
// as a harness's headers helper answers them, a JSON object: each header's
// value read from its file in files, its CRs and newlines dropped and its
// backslashes, quotes and tabs escaped. It holds the files' paths and no
// value; a file it cannot read, or one holding another control character,
// fails it. A profile writes it as a file of its own under config, which the
// harness runs with /bin/sh each time it connects (no provisioned file is
// executable): a connector's headers_file.
func HeadersScript(name string, files map[string]string) string {
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n# The headers of MCP server %s, read from their files each time the\n# harness connects: no value is in its configuration or environment.\nset -eu\nLC_ALL=C\nexport LC_ALL\n", name)
	b.WriteString(`die() { echo "$1" >&2; exit 1; }` + "\n")
	// ctl is $1's control characters but tabs, which JSON cannot hold as
	// esc leaves them.
	b.WriteString(`ctl() { printf '%s' "$1" | tr -d '\t -~\200-\377'; }` + "\n")
	b.WriteString(`esc() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/` + "\t" + `/\\t/g'; }` + "\n")
	// Each value is read into a variable of its own, never inside another
	// command's arguments, so a read that fails fails the script.
	for i, k := range names {
		f := ShellQuote(files[k])
		unreadable := ShellQuote("cannot read the file of header " + k)
		fmt.Fprintf(&b, "[ -r %s ] || die %s\n", f, unreadable)
		fmt.Fprintf(&b, "v%d=$(tr -d '\\r\\n' <%s) || die %s\n", i, f, unreadable)
		fmt.Fprintf(&b, "[ -z \"$(ctl \"$v%d\")\" ] || die %s\n", i, ShellQuote("the file of header "+k+" holds a control character"))
		fmt.Fprintf(&b, "v%d=$(esc \"$v%d\")\n", i, i)
	}
	b.WriteString("printf '{'\n")
	for i, k := range names {
		sep := ","
		if i == 0 {
			sep = ""
		}
		fmt.Fprintf(&b, "printf '%s\"%%s\":\"%%s\"' %s \"$v%d\"\n", sep, ShellQuote(k), i)
	}
	b.WriteString("printf '}\\n'\n")
	return b.String()
}

// HeaderCommand is a shell command that prints the value of a header read
// from file as HeadersScript reads one: its CRs and newlines dropped. A file
// it cannot read, or one holding another control character but a tab, fails
// it, printing nothing. It holds the file's path and no value: a harness
// whose configuration takes a command per header runs it each time it
// connects.
func HeaderCommand(file string) string {
	return `export LC_ALL=C; v=$(tr -d '\r\n' <` + ShellQuote(file) + `) && [ -z "$(printf '%s' "$v" | tr -d '\t -~\200-\377')" ] && printf '%s' "$v"`
}

// ShellQuote is s as one shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
