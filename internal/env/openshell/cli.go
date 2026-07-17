package openshell

import (
	"os/exec"
	"strings"
)

// spawnOpenShellCli is the default CliRunner: it spawns a real `openshell …`
// process, capturing stdout/stderr and the exit code. A spawn failure surfaces
// as code -1 with the error text in Stderr.
func spawnOpenShellCli(argv []string) CliResult {
	if len(argv) == 0 {
		return CliResult{Code: -1, Stderr: "empty argv"}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return CliResult{Code: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}
		}
		return CliResult{Code: -1, Stdout: stdout.String(), Stderr: err.Error()}
	}
	return CliResult{Code: 0, Stdout: stdout.String(), Stderr: stderr.String()}
}

// stringFrom reads a string value from a policy Extra map (empty when absent).
func stringFrom(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// stringOr reads a string value from a policy Extra map, defaulting when absent
// or empty.
func stringOr(m map[string]interface{}, key, def string) string {
	if s := stringFrom(m, key); s != "" {
		return s
	}
	return def
}

// intOr reads an int value from a policy Extra map, defaulting when absent. It
// accepts int and float64 (the JSON-decoded numeric form).
func intOr(m map[string]interface{}, key string, def int) int {
	if m == nil {
		return def
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}

// scrapeEndpointsFrom extracts optional scrape endpoints from a policy Extra map.
// It accepts a []ScrapeEndpoint directly (the native shape). Absent/other types
// yield nil, which emits no scrape lane.
func scrapeEndpointsFrom(m map[string]interface{}) []ScrapeEndpoint {
	if m == nil {
		return nil
	}
	eps, _ := m["scrapeEndpoints"].([]ScrapeEndpoint)
	return eps
}
