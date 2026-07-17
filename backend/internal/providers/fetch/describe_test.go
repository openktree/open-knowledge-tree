package fetch

import (
	"reflect"
	"testing"
)

// TestResolutionProviderInterfaceCompliance is a compile-time
// guard: the tests below rely on every concrete provider
// satisfying the ResolutionProvider interface (which now
// includes Describe). A provider that forgets to implement
// Describe would still compile in the strategy and silently
// break the /sources/providers endpoint, so we pin the
// interface assertion here.
func TestResolutionProviderInterfaceCompliance(t *testing.T) {
	var _ ResolutionProvider = (*FetchResolutionProvider)(nil)
	var _ ResolutionProvider = (*UnpaywallResolutionProvider)(nil)
}

func TestFetchResolutionProviderDescribe(t *testing.T) {
	p := NewFetchResolutionProvider()
	d := p.Describe()

	if d.Name == "" {
		t.Error("expected non-empty name for FetchResolutionProvider")
	}
	if d.Description == "" {
		t.Error("expected non-empty description for FetchResolutionProvider")
	}
	if d.Requires != "" {
		t.Errorf("expected empty Requires for the always-on fetch provider, got %q", d.Requires)
	}
	if !d.Configured {
		t.Error("expected FetchResolutionProvider to report Configured=true (no env vars required)")
	}
	if !reflect.DeepEqual(d.Supports, []string{"url", "doi"}) {
		t.Errorf("expected Supports=[url, doi], got %v", d.Supports)
	}
	if d.Timeout == "" {
		t.Error("expected a non-empty Timeout string")
	}
	if d.Notes == "" {
		t.Error("expected a non-empty Notes field describing the strategy behavior")
	}
}

func TestUnpaywallResolutionProviderDescribe(t *testing.T) {
	cases := []struct {
		name        string
		email       string
		wantNil     bool
		wantConfig  bool
		wantRequire string
	}{
		{
			name:        "no email is not configured",
			email:       "",
			wantNil:     true,
			wantRequire: "UNPAYWALL_EMAIL",
		},
		{
			name:        "valid email is configured",
			email:       "user@example.com",
			wantNil:     false,
			wantConfig:  true,
			wantRequire: "UNPAYWALL_EMAIL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewUnpaywallResolutionProvider(tc.email)
			if (p == nil) != tc.wantNil {
				t.Fatalf("NewUnpaywallResolutionProvider(%q) nil=%v, want nil=%v",
					tc.email, p == nil, tc.wantNil)
			}
			if p == nil {
				return
			}
			d := p.Describe()
			if d.Name == "" {
				t.Error("expected non-empty name for UnpaywallResolutionProvider")
			}
			if d.Description == "" {
				t.Error("expected non-empty description for UnpaywallResolutionProvider")
			}
			if d.Requires != tc.wantRequire {
				t.Errorf("Requires = %q, want %q", d.Requires, tc.wantRequire)
			}
			if d.Configured != tc.wantConfig {
				t.Errorf("Configured = %v, want %v", d.Configured, tc.wantConfig)
			}
			if !reflect.DeepEqual(d.Supports, []string{"doi"}) {
				t.Errorf("expected Supports=[doi], got %v", d.Supports)
			}
			if d.Timeout == "" {
				t.Error("expected a non-empty Timeout string")
			}
			if d.Notes == "" {
				t.Error("expected a non-empty Notes field describing the fall-through behavior")
			}
		})
	}
}
