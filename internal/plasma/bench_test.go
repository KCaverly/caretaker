package plasma

import "testing"

func BenchmarkRenderFrame(b *testing.B) {
	f, err := New(Options{Pattern: "classic", Palette: "aurora", Charset: "dots", Speed: 0.3})
	if err != nil {
		b.Fatal(err)
	}
	// A typical panel: 40% of a 120-col terminal, ~30 rows of body.
	for b.Loop() {
		f.Advance(0.15)
		_ = f.Render(44, 28)
	}
}
