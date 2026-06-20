package filetype

import "testing"

func TestRouteExtHCL(t *testing.T) {
	for _, ext := range []string{".tf", ".hcl"} {
		r := RouteExt(ext)
		if r.Type != Code || r.Grammar != "hcl" {
			t.Fatalf("RouteExt(%q) = %+v, want {Code hcl}", ext, r)
		}
	}
}

func TestRouteExtText(t *testing.T) {
	if r := RouteExt(".md"); r.Type != Text {
		t.Fatalf("RouteExt(.md) = %+v, want Text", r)
	}
}
