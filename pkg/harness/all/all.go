// Package all blank-imports every built-in harness profile so that
// harness.For(name) resolves them. Import it for its side effects:
//
//	import _ "github.com/olesho/harness-wrapper/pkg/harness/all"
//
// Callers that only need a specific harness may blank-import just that
// subpackage (e.g. pkg/harness/claude) instead.
package all

import (
	_ "github.com/olesho/harness-wrapper/pkg/harness/claude"
	_ "github.com/olesho/harness-wrapper/pkg/harness/gemini"
)
