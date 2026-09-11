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

// TestSkewError pins which version pairs are a refusal, and that each
// mode's message names what the load would have done. The guard needs a
// real release gap; anything unparseable is not refused.
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
		for _, exportData := range []bool{false, true} {
			err := skewError(tc.self, tc.tool, exportData)
			if (err != nil) != tc.refuse {
				t.Errorf("skewError(%q, %q, %v) = %v; want refusal=%v", tc.self, tc.tool, exportData, err, tc.refuse)
				continue
			}
			if err == nil {
				continue
			}
			if !strings.Contains(err.Error(), tc.self) || !strings.Contains(err.Error(), tc.tool) {
				t.Errorf("skewError(%q, %q, %v) names neither version: %v", tc.self, tc.tool, exportData, err)
			}
			want, unwanted := "analyse a fraction", "end the process"
			if exportData {
				want, unwanted = "end the process", "analyse a fraction"
			}
			if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), unwanted) {
				t.Errorf("skewError(exportData=%v) does not describe its own mode: %v", exportData, err)
			}
		}
	}
}

// TestToolchainSkew_RefusesBothLoads pins that the engine refuses under a
// newer toolchain in either mode instead of calling into go/packages: the
// export-data load would end the process (#35), and the default load
// would return a graph missing most of the module without saying so. The
// skew is simulated, since the test binary is built by the go command
// that would run the load.
func TestToolchainSkew_RefusesBothLoads(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "exportdeps"))
	if err != nil {
		t.Fatal(err)
	}
	stubGoVersion(t, "go1.99.0")

	for _, exportData := range []bool{false, true} {
		_, err := NewEngineWithOptions(dir, EngineOptions{SkipCgo: true, DepsFromExportData: exportData})
		if err == nil {
			t.Errorf("DepsFromExportData=%v: load succeeded under a newer toolchain; want a refusal before the load", exportData)
			continue
		}
		for _, want := range []string{runtime.Version(), "go1.99.0", "#35"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("DepsFromExportData=%v: refusal does not mention %q: %v", exportData, want, err)
			}
		}
	}
}

// TestToolchainSkew_NoSkewLoads pins that the guard does not refuse the
// real machine, where the program and the go command are the same
// release: goVersionOf is not stubbed here, in either mode.
func TestToolchainSkew_NoSkewLoads(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "exportdeps"))
	if err != nil {
		t.Fatal(err)
	}
	for _, exportData := range []bool{false, true} {
		if _, err := NewEngineWithOptions(dir, EngineOptions{SkipCgo: true, DepsFromExportData: exportData}); err != nil {
			t.Fatalf("DepsFromExportData=%v: load refused without skew: %v", exportData, err)
		}
	}
}
