package fetch

import "testing"

func TestClassifyURL(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantType SourceType
		wantVal  string
		wantDOI  string
	}{
		{
			name:     "bare doi",
			input:    "10.1038/nature12373",
			wantType: SourceDOI,
			wantVal:  "10.1038/nature12373",
			wantDOI:  "10.1038/nature12373",
		},
		{
			name:     "https doi",
			input:    "https://doi.org/10.1038/nature12373",
			wantType: SourceDOI,
			wantVal:  "10.1038/nature12373",
			wantDOI:  "10.1038/nature12373",
		},
		{
			name:     "http doi",
			input:    "http://doi.org/10.1038/nature12373",
			wantType: SourceDOI,
			wantVal:  "10.1038/nature12373",
			wantDOI:  "10.1038/nature12373",
		},
		{
			name:     "dx doi",
			input:    "https://dx.doi.org/10.1038/nature12373",
			wantType: SourceDOI,
			wantVal:  "10.1038/nature12373",
			wantDOI:  "10.1038/nature12373",
		},
		{
			name:     "plain https url",
			input:    "https://example.com/foo",
			wantType: SourceURL,
			wantVal:  "https://example.com/foo",
		},
		{
			name:     "plain http url",
			input:    "http://example.com/foo",
			wantType: SourceURL,
			wantVal:  "http://example.com/foo",
		},
		{
			name:     "schemeless input falls back to url",
			input:    "example.com/foo",
			wantType: SourceURL,
			wantVal:  "example.com/foo",
		},
		{
			name:     "whitespace is trimmed",
			input:    "   10.1038/nature12373   ",
			wantType: SourceDOI,
			wantVal:  "10.1038/nature12373",
			wantDOI:  "10.1038/nature12373",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyURL(tc.input)
			if got.Type != tc.wantType {
				t.Errorf("type: want %q, got %q", tc.wantType, got.Type)
			}
			if got.Value != tc.wantVal {
				t.Errorf("value: want %q, got %q", tc.wantVal, got.Value)
			}
			if got.DOI != tc.wantDOI {
				t.Errorf("doi: want %q, got %q", tc.wantDOI, got.DOI)
			}
		})
	}
}
