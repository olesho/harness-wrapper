package contract

import _ "embed"

// Schema is the JSON Schema of every type that crosses a process boundary or
// is journaled, and of every observation's data, under $defs. It is generated
// from the Go types (TestSchema), never kept by hand beside them.
//
//go:embed schema.json
var Schema []byte
