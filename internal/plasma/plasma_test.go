package plasma

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func defaultOpts() Options {
	return Options{Pattern: "classic", Palette: "aurora", Charset: "dots", Speed: 0.3}
}

func TestRenderDimensions(t *testing.T) {
	// Every pattern must fill the requested frame exactly: the deck's box
	// helper assumes pre-sized lines and won't fix ragged ones.
	for name := range patterns {
		o := defaultOpts()
		o.Pattern = name
		f, err := New(o)
		if err != nil {
			t.Fatalf("New(%s): %v", name, err)
		}
		lines := f.Render(30, 8)
		if len(lines) != 8 {
			t.Fatalf("%s: got %d lines, want 8", name, len(lines))
		}
		for i, ln := range lines {
			if w := lipgloss.Width(ln); w != 30 {
				t.Errorf("%s line %d: width %d, want 30", name, i, w)
			}
		}
	}
}

func TestValidateRejectsUnknownNames(t *testing.T) {
	for _, o := range []Options{
		{Pattern: "nope", Palette: "aurora", Charset: "dots"},
		{Pattern: "classic", Palette: "nope", Charset: "dots"},
		{Pattern: "classic", Palette: "aurora", Charset: "nope"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) should reject the unknown name", o)
		}
	}
}

func TestZeroSpeedFreezesTheField(t *testing.T) {
	o := defaultOpts()
	o.Speed = 0
	f, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if f.Animated() {
		t.Fatal("zero speed should report not animated")
	}
	before := strings.Join(f.Render(20, 5), "\n")
	f.Advance(10)
	if after := strings.Join(f.Render(20, 5), "\n"); after != before {
		t.Fatal("a frozen field must render identically after Advance")
	}
}

func TestAdvanceMovesTheField(t *testing.T) {
	f, err := New(defaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	before := strings.Join(f.Render(20, 5), "\n")
	f.Advance(2)
	if after := strings.Join(f.Render(20, 5), "\n"); after == before {
		t.Fatal("advancing an animated field should change the frame")
	}
}
