package analysis

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubGoVersion makes goVersionOf report v for the duration of the test.
func stubGoVersion(t *testing.T, v string) {
	t.Helper()
	prev := goVersionOf
	goVersionOf = func([]string, string) (string, error) { return v, nil }
	t.Cleanup(func() { goVersionOf = prev })
}

// TestSkewError pins which version pairs are a refusal. The fatal this
// guards needs go/packages to find export data it cannot read, which
// takes a real release gap; anything unparseable is not refused.
func TestSkewError(t *testing.T) {
	for _, tc := range []struct {
		self, tool string
		refuse     bool
	}{
		{"go1.26.7", "go1.27.1", true},
		{"go1.26.0", "go1.99.0", true},
		{"go1.27.1", "go1.27.1", false},
		{"go1.27.1", "go1.26.7", false}, // newer program, older toolchain: readable
		{"go1.28.0", "go1.27.9", false},
		{"devel +abc123", "go1.27.1", false}, // unparseable self
		{"go1.26.7", "devel +abc123", false}, // unparseable toolchain
		{"", "", false},
	} {
		err := skewError(tc.self, tc.tool)
		if (err != nil) != tc.refuse {
			t.Errorf("skewError(%q, %q) = %v; want refusal=%v", tc.self, tc.tool, err, tc.refuse)
		}
		if err != nil && (!strings.Contains(err.Error(), tc.self) || !strings.Contains(err.Error(), tc.tool)) {
			t.Errorf("skewError(%q, %q) names neither version: %v", tc.self, tc.tool, err)
		}
	}
}

// TestToolchainSkew_RefusesExportDataLoad pins that the engine refuses
// the export-data load under skew instead of calling into go/packages,
// which would end the process (#35), and that the default load is not
// refused. The skew is simulated: reproducing it needs two toolchains,
// and the test binary is built by the go command that would run the load.
func TestToolchainSkew_RefusesExportDataLoad(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "exportdeps"))
	if err != nil {
		t.Fatal(err)
	}
	stubGoVersion(t, "go1.99.0")

	if _, err := NewEngineWithOptions(dir, EngineOptions{SkipCgo: true, DepsFromExportData: true}); err == nil {
		t.Error("DepsFromExportData load succeeded under a newer toolchain; want a refusal before the load")
	} else {
		for _, want := range []string{runtime.Version(), "go1.99.0", "#35"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not mention %q: %v", want, err)
			}
		}
	}

	// The default load is unaffected: it completes under skew today, and
	// refusing it too is a wider decision than this fix.
	eng, err := NewEngineWithOptions(dir, EngineOptions{SkipCgo: true})
	if err != nil {
		t.Fatalf("default load refused under a newer toolchain: %v", err)
	}
	if len(eng.Packages()) == 0 {
		t.Error("default load returned no packages")
	}
}

// TestToolchainSkew_NoSkewLoads pins that the guard does not refuse the
// real machine, where the program and the go command are the same
// release: goVersionOf is not stubbed here.
func TestToolchainSkew_NoSkewLoads(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "exportdeps"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngineWithOptions(dir, EngineOptions{SkipCgo: true, DepsFromExportData: true}); err != nil {
		t.Fatalf("export-data load refused without skew: %v", err)
	}
}
