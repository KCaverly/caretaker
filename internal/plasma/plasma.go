// Package plasma renders the deck's ambient panel: a classic demoscene
// sum-of-sines plasma mapped onto a character-density ramp and a smooth
// gruvbox color ramp. A frame is a pure function of (cell, phase), so the
// panel needs no stored pixel state — the model keeps one float and the
// renderer does the rest.
package plasma

import (
	"fmt"
	"image/color"
	"math"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

// Options selects a variant by name; names are validated so a config typo
// fails loudly at startup instead of silently falling back.
type Options struct {
	Pattern string  // classic | waves | interference | lava | ripple
	Palette string  // aurora | ember | mono
	Charset string  // dots | shade | blocks
	Speed   float64 // phase units per second; 0 freezes the frame
}

// evalFunc returns a value in [0,1] for cell (x,y) at phase t. y arrives
// pre-doubled to correct the ~2:1 terminal cell aspect, and (cx,cy) is the
// frame center in those same coordinates.
type evalFunc func(x, y, t, cx, cy float64) float64

var patterns = map[string]evalFunc{
	"classic": func(x, y, t, cx, cy float64) float64 {
		v := math.Sin(x*0.12 + t)
		v += math.Sin(y*0.16 + t*1.3)
		v += math.Sin((x+y)*0.09 + t*0.7)
		v += math.Sin(math.Hypot(x-cx, y-cy)*0.14 + t)
		return (v + 4) / 8
	},
	"waves": func(x, y, t, cx, cy float64) float64 {
		v := math.Sin(y*0.30 + x*0.04 + t)
		v += math.Sin(y*0.22 - x*0.03 - t*0.7)
		v += 0.5 * math.Sin(x*0.10+t*0.4)
		return (v + 2.5) / 5
	},
	"interference": func(x, y, t, cx, cy float64) float64 {
		v := math.Sin(x*0.25+t) * math.Sin(y*0.25-t*0.8)
		v += math.Sin((x-y)*0.11 + t*0.6)
		return (v + 2) / 4
	},
	"lava": func(x, y, t, cx, cy float64) float64 {
		// two blob centers orbiting the frame center
		c1x, c1y := cx+cx*0.6*math.Sin(t*0.5), cy+cy*0.6*math.Cos(t*0.4)
		c2x, c2y := cx+cx*0.6*math.Cos(t*0.3), cy+cy*0.6*math.Sin(t*0.6)
		v := math.Sin(math.Hypot(x-c1x, y-c1y)*0.16 - t)
		v += math.Sin(math.Hypot(x-c2x, y-c2y)*0.13 + t*0.8)
		return (v + 2) / 4
	},
	"ripple": func(x, y, t, cx, cy float64) float64 {
		v := math.Sin(math.Hypot(x-cx, y-cy)*0.35 - t*2)
		v += 0.4 * math.Sin(x*0.08+t*0.5)
		return (v + 1.4) / 2.8
	},
}

// Gruvbox anchors (same hexes as the TUI's palette in view.go). Ramps run
// dark→bright so the pattern's low end melts into the terminal background.
type rgb struct{ r, g, b float64 }

var (
	gbBg     = rgb{29, 32, 33}    // #1D2021 bg0_h
	gbFaint  = rgb{102, 92, 84}   // #665C54 bg3
	gbDim    = rgb{146, 131, 116} // #928374 gray
	gbBlue   = rgb{131, 165, 152} // #83A598
	gbPurple = rgb{211, 134, 155} // #D3869B
	gbYellow = rgb{250, 189, 47}  // #FABD2F
	gbRed    = rgb{251, 73, 52}   // #FB4934
	gbFg     = rgb{235, 219, 178} // #EBDBB2
)

var palettes = map[string][]rgb{
	"aurora": {gbBg, gbFaint, gbBlue, gbPurple},
	"ember":  {gbBg, gbFaint, gbYellow, gbRed},
	"mono":   {gbBg, gbFaint, gbDim, gbFg},
}

var charsets = map[string][]rune{
	"dots":   []rune(" ⠄⠆⠖⠶⡶⣶⣿"),
	"shade":  []rune(" ·:!*#%@"),
	"blocks": []rune(" ░▒▓█"),
}

// rampSteps is the color resolution. 48 truecolor steps read as a continuous
// gradient while keeping the precomputed style table small.
const rampSteps = 48

// Validate reports whether the named variants exist, so config loading can
// reject typos with the valid names in the error.
func Validate(o Options) error {
	if _, ok := patterns[o.Pattern]; !ok {
		return fmt.Errorf("unknown plasma pattern %q (valid: %s)", o.Pattern, names(patterns))
	}
	if _, ok := palettes[o.Palette]; !ok {
		return fmt.Errorf("unknown plasma palette %q (valid: %s)", o.Palette, names(palettes))
	}
	if _, ok := charsets[o.Charset]; !ok {
		return fmt.Errorf("unknown plasma charset %q (valid: %s)", o.Charset, names(charsets))
	}
	return nil
}

func names[V any](m map[string]V) string {
	ns := make([]string, 0, len(m))
	for n := range m {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return strings.Join(ns, ", ")
}

// Field is a running plasma instance: the resolved variant plus its phase.
type Field struct {
	eval   evalFunc
	styles []lipgloss.Style // precomputed color ramp, dark→bright
	runes  []rune           // density ramp, sparse→solid
	speed  float64
	t      float64
}

// New resolves an Options into a renderable Field.
func New(o Options) (*Field, error) {
	if err := Validate(o); err != nil {
		return nil, err
	}
	anchors := palettes[o.Palette]
	styles := make([]lipgloss.Style, rampSteps)
	for i := range styles {
		styles[i] = lipgloss.NewStyle().Foreground(lerp(anchors, float64(i)/(rampSteps-1)))
	}
	return &Field{
		eval:   patterns[o.Pattern],
		styles: styles,
		runes:  charsets[o.Charset],
		speed:  o.Speed,
	}, nil
}

// lerp interpolates through the anchor colors at position pos in [0,1].
func lerp(anchors []rgb, pos float64) color.Color {
	segs := float64(len(anchors) - 1)
	s := min(int(pos*segs), len(anchors)-2)
	f := pos*segs - float64(s)
	a, b := anchors[s], anchors[s+1]
	return color.RGBA{
		R: uint8(a.r + (b.r-a.r)*f),
		G: uint8(a.g + (b.g-a.g)*f),
		B: uint8(a.b + (b.b-a.b)*f),
		A: 0xFF,
	}
}

// Animated reports whether the field moves at all (a zero speed freezes it,
// letting callers skip the animation tick entirely).
func (f *Field) Animated() bool { return f.speed > 0 }

// Advance moves the phase forward by dt seconds of wall time.
func (f *Field) Advance(dt float64) { f.t += dt * f.speed }

// Render draws one w×h frame as styled lines, each exactly w columns wide.
// Adjacent cells sharing a ramp color are styled as one run to keep the
// escape-code volume (and per-frame allocation) down.
func (f *Field) Render(w, h int) []string {
	lines := make([]string, h)
	cx, cy := float64(w)/2, float64(h) // center, y pre-doubled for cell aspect
	var b, run strings.Builder
	for y := 0; y < h; y++ {
		b.Reset()
		runIdx := -1
		flush := func() {
			if run.Len() > 0 {
				b.WriteString(f.styles[runIdx].Render(run.String()))
				run.Reset()
			}
		}
		for x := 0; x < w; x++ {
			v := f.eval(float64(x), float64(y)*2, f.t, cx, cy)
			v = math.Max(0, math.Min(1, v))
			ci := int(v * float64(len(f.styles)-1))
			if ci != runIdx {
				flush()
				runIdx = ci
			}
			run.WriteRune(f.runes[int(v*float64(len(f.runes)-1))])
		}
		flush()
		lines[y] = b.String()
	}
	return lines
}
