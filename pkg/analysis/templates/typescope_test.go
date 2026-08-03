package templates

import (
	"strings"
	"testing"
)

// Fixtures model the bellui pre-v1 bug classes from issue #8: paired
// lifecycle calls across methods (#167/#120) and capture-at-create vs
// re-read-at-refresh (#203).

const pairedFixture = `package testpkg

type Animation struct{}

func (a *Animation) Start() {}
func (a *Animation) Stop()  {}

// Widget starts its animation and stops it in a sibling method: OK.
type Widget struct {
	anim *Animation
}

func (w *Widget) Show() { w.anim.Start() }
func (w *Widget) Hide() { w.anim.Stop() }

// Leaky starts its animation and never stops it anywhere: finding.
type Leaky struct {
	anim *Animation
}

func (l *Leaky) Show() { l.anim.Start() }

// SplitWidget starts in a method, stops in the renderer it constructs
// (the widget→renderer link): OK.
type SplitWidget struct {
	anim *Animation
}

type splitRenderer struct {
	w *SplitWidget
}

func (r *splitRenderer) Destroy() { r.w.anim.Stop() }

func (w *SplitWidget) Show() { w.anim.Start() }

func (w *SplitWidget) CreateRenderer() *splitRenderer {
	return &splitRenderer{w: w}
}

// TwoFields starts both animations but stops only one: one finding,
// on the field with no Stop.
type TwoFields struct {
	spin *Animation
	fade *Animation
}

func (t *TwoFields) Show() {
	t.spin.Start()
	t.fade.Start()
}

func (t *TwoFields) Hide() {
	t.spin.Stop()
}
`

func TestPairedCalls(t *testing.T) {
	engine := setupProject(t, pairedFixture)

	tmpl, err := NewPairedCalls("$x.Start()", "$x.Stop()")
	if err != nil {
		t.Fatal(err)
	}

	findings := RunWholeProgram(engine, tmpl)

	if len(findings) != 2 {
		for _, f := range findings {
			t.Logf("finding: %s (%s:%d)", f.Message, f.Location.File, f.Location.Line)
		}
		t.Fatalf("got %d findings, want 2 (Leaky.Show and TwoFields fade)", len(findings))
	}

	var haveLeaky, haveFade bool
	for _, f := range findings {
		if f.Kind != "unpaired_call" {
			t.Errorf("finding kind = %q, want unpaired_call", f.Kind)
		}
		if strings.Contains(f.Message, "Leaky.Show") {
			haveLeaky = true
		}
		if strings.Contains(f.Message, "fade") {
			haveFade = true
		}
	}
	if !haveLeaky {
		t.Error("missing finding for Leaky.Show (no Stop anywhere)")
	}
	if !haveFade {
		t.Error("missing finding for TwoFields.fade (only spin is stopped)")
	}
}

func TestPairedCalls_ConstructedScopeDisabled(t *testing.T) {
	engine := setupProject(t, pairedFixture)

	tmpl, err := NewPairedCalls("$x.Start()", "$x.Stop()")
	if err != nil {
		t.Fatal(err)
	}
	tmpl.IncludeConstructed = false

	findings := RunWholeProgram(engine, tmpl)

	// Without companion-type search, SplitWidget's Start (stopped only in
	// the renderer) is also flagged.
	var haveSplit bool
	for _, f := range findings {
		if strings.Contains(f.Message, "SplitWidget.Show") {
			haveSplit = true
		}
	}
	if !haveSplit {
		t.Error("with IncludeConstructed=false, expected SplitWidget.Show to be flagged")
	}
}

const staleCaptureFixture = `package testpkg

type canvasObject struct{}

// Bar captures Segments in CreateRenderer and never re-reads it in
// Refresh: finding. Title is re-read: OK.
type Bar struct {
	Segments []int
	Title    string
}

type barRenderer struct {
	bar      *Bar
	segments []int
	title    string
}

func (r *barRenderer) Refresh() {
	r.title = r.bar.Title
}

func (b *Bar) CreateRenderer() *barRenderer {
	return &barRenderer{
		bar:      b,
		segments: b.Segments,
		title:    b.Title,
	}
}

// Fresh re-reads everything in Refresh: no findings.
type Fresh struct {
	Items []string
}

type freshRenderer struct {
	f     *Fresh
	items []string
}

func (r *freshRenderer) Refresh() {
	r.items = r.f.Items
}

func (f *Fresh) CreateRenderer() *freshRenderer {
	return &freshRenderer{f: f, items: f.Items}
}

// NoRenderer has a CreateRenderer-shaped method but nothing in scope
// defines Refresh: skipped, no findings.
type NoRenderer struct {
	Data []int
}

func (n *NoRenderer) CreateRenderer() []int {
	return n.Data
}
`

func TestStaleCapture(t *testing.T) {
	engine := setupProject(t, staleCaptureFixture)

	tmpl, err := NewStaleCapture("CreateRenderer", "Refresh")
	if err != nil {
		t.Fatal(err)
	}

	findings := RunWholeProgram(engine, tmpl)

	if len(findings) != 1 {
		for _, f := range findings {
			t.Logf("finding: %s (%s:%d)", f.Message, f.Location.File, f.Location.Line)
		}
		t.Fatalf("got %d findings, want 1 (Bar.Segments)", len(findings))
	}

	f := findings[0]
	if f.Kind != "stale_capture" {
		t.Errorf("kind = %q, want stale_capture", f.Kind)
	}
	if !strings.Contains(f.Message, "Bar.Segments") {
		t.Errorf("message %q should name Bar.Segments", f.Message)
	}
	if !strings.Contains(f.Message, "barRenderer.Refresh") {
		t.Errorf("message %q should name the re-read scope barRenderer.Refresh", f.Message)
	}
}

func TestStaleCapture_UnexportedSkippedByDefault(t *testing.T) {
	src := `package testpkg

type W struct {
	items []int
}

type wRenderer struct {
	items []int
}

func (r *wRenderer) Refresh() {}

func (w *W) CreateRenderer() *wRenderer {
	return &wRenderer{items: w.items}
}
`
	engine := setupProject(t, src)

	tmpl, err := NewStaleCapture("CreateRenderer", "Refresh")
	if err != nil {
		t.Fatal(err)
	}
	if got := RunWholeProgram(engine, tmpl); len(got) != 0 {
		t.Fatalf("exported-only default: got %d findings, want 0", len(got))
	}

	tmpl.ExportedOnly = false
	if got := RunWholeProgram(engine, tmpl); len(got) != 1 {
		t.Fatalf("ExportedOnly=false: got %d findings, want 1", len(got))
	}
}
