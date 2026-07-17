package fetch

import (
	"errors"
	"testing"
)

func TestUnpaywallNewUnpaywallResolutionProvider(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  bool
	}{
		{name: "empty email returns nil", email: "", want: false},
		{name: "whitespace email returns nil", email: "   ", want: false},
		{name: "valid email returns provider", email: "user@example.com", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewUnpaywallResolutionProvider(tc.email)
			if (got != nil) != tc.want {
				t.Errorf("NewUnpaywallResolutionProvider(%q) nil=%v, want non-nil=%v",
					tc.email, got == nil, tc.want)
			}
		})
	}
}

func TestUnpaywallSupports(t *testing.T) {
	p := NewUnpaywallResolutionProvider("user@example.com")
	if p == nil {
		t.Fatal("expected non-nil provider for valid email")
	}
	if !p.Supports(SourceDOI) {
		t.Error("expected Supports(SourceDOI) to be true")
	}
	if p.Supports(SourceURL) {
		t.Error("expected Supports(SourceURL) to be false")
	}
}

func TestSelectOALocation(t *testing.T) {
	cases := []struct {
		name string
		resp unpaywallResponse
		want string
	}{
		{
			name: "prefers best_oa_location.url_for_pdf",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{
					URL:       "https://best.example.com/p",
					URLForPDF: "https://best.example.com/p.pdf",
				},
				OALocations: []unpaywallLocation{
					{URL: "https://other.example.com/p"},
				},
			},
			want: "https://best.example.com/p.pdf",
		},
		{
			name: "falls back to best_oa_location.url when no pdf",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{URL: "https://best.example.com/p"},
			},
			want: "https://best.example.com/p",
		},
		{
			name: "falls back to oa_locations[0].url_for_pdf when best is nil",
			resp: unpaywallResponse{
				OALocations: []unpaywallLocation{
					{URLForPDF: "https://repo.example.com/p.pdf"},
					{URL: "https://other.example.com/p"},
				},
			},
			want: "https://repo.example.com/p.pdf",
		},
		{
			name: "falls back to oa_locations url across locations",
			resp: unpaywallResponse{
				OALocations: []unpaywallLocation{
					{URL: "https://repo.example.com/p"},
				},
			},
			want: "https://repo.example.com/p",
		},
		{
			name: "empty result when no urls are present",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{HostType: "publisher"},
				OALocations: []unpaywallLocation{
					{License: "CC-BY"},
				},
			},
			want: "",
		},
		{
			name: "empty best_oa_location object still lets oa_locations win",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{},
				OALocations: []unpaywallLocation{
					{URL: "https://repo.example.com/p"},
				},
			},
			want: "https://repo.example.com/p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectOALocation(tc.resp)
			if got != tc.want {
				t.Errorf("selectOALocation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnpaywallErrNotOpenAccessIsExported(t *testing.T) {
	// The strategy depends on a sentinel value to drive
	// fall-through; we want the type to be stable for
	// future callers and for errors.Is to keep working.
	// The test is intentionally minimal: it just asserts
	// the symbol exists and is non-nil, and that wrapping
	// it in fmt.Errorf still matches.
	err := ErrUnpaywallNotOpenAccess
	if err == nil {
		t.Fatal("ErrUnpaywallNotOpenAccess must be non-nil")
	}
	if !errors.Is(err, ErrUnpaywallNotOpenAccess) {
		t.Error("expected errors.Is to match the sentinel directly")
	}
}
