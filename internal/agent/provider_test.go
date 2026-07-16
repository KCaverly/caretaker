package agent

import "testing"

func TestProviderValid(t *testing.T) {
	for _, p := range []Provider{Claude, Codex} {
		if !p.Valid() {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []Provider{"", "other"} {
		if p.Valid() {
			t.Errorf("%q should be invalid", p)
		}
	}
}
