//go:build e2e

package e2e

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/asilluron/lcars-api/internal/config"
	"github.com/asilluron/lcars-api/internal/natsapi"
)

// The e2e suite must exercise the whole external surface of the server:
// every HTTP path in the OpenAPI document, every NATS subject, and every
// setting. The helpers record what the tests use, and checkCoverage fails
// a full run if anything was never used, so new endpoints and settings can't
// land without an e2e test.
const (
	pathCoverage     = "HTTP path"
	subjectCoverage  = "NATS subject"
	settingsCoverage = "setting (environment variable)"
)

var (
	coverageMu sync.Mutex
	covered    = map[string]map[string]bool{}
)

// cover records that the tests used name.
func cover(kind, name string) {
	coverageMu.Lock()
	defer coverageMu.Unlock()
	if covered[kind] == nil {
		covered[kind] = map[string]bool{}
	}
	covered[kind][name] = true
}

// checkCoverage returns 1, after printing what is missing, if a full run
// didn't use every path, subject and setting. Runs filtered with -run or
// -skip are not checked.
func checkCoverage() int {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return 0
		}
	}

	paths, err := documentedPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: coverage:", err)
		return 1
	}
	want := map[string][]string{
		pathCoverage:     paths,
		subjectCoverage:  natsapi.Subjects,
		settingsCoverage: append(config.EnvVars(), "LCARS_CONFIG"),
	}

	coverageMu.Lock()
	defer coverageMu.Unlock()
	var missing []string
	for kind, names := range want {
		for _, name := range names {
			if !covered[kind][name] {
				missing = append(missing, kind+" "+name)
			}
		}
	}
	if len(missing) == 0 {
		return 0
	}
	slices.Sort(missing)
	fmt.Fprintf(os.Stderr, "\ne2e: FAIL: not exercised by any e2e test (every route, subject and setting must be):\n  %s\n",
		strings.Join(missing, "\n  "))
	return 1
}

// documentedPaths lists the paths in the OpenAPI document.
func documentedPaths() ([]string, error) {
	data, err := os.ReadFile("../internal/server/api/openapi.json")
	if err != nil {
		return nil, err
	}
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse OpenAPI document: %w", err)
	}
	var paths []string
	for p := range spec.Paths {
		paths = append(paths, p)
	}
	return paths, nil
}
