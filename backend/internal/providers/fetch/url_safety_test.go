package fetch

import (
	"errors"
	"net"
	"testing"
)

// TestValidateFetchURL covers the SSRF guard's accept/reject
// decisions. The DNS lookup is stubbed so the tests don't
// depend on a real resolver.
func TestValidateFetchURL(t *testing.T) {
	// Save and restore the real resolver.
	origLookup := lookupAllIPs
	defer func() { lookupAllIPs = origLookup }()

	cases := []struct {
		name     string
		url      string
		lookup   func(host string) ([]net.IP, error)
		wantErr  bool
		wantSent error // when set, the error must match errors.Is(err, wantSent)
	}{
		{
			name:    "empty url rejected",
			url:     "",
			lookup:  func(string) ([]net.IP, error) { return nil, nil },
			wantErr: true,
		},
		{
			name:    "non-http scheme rejected",
			url:     "file:///etc/passwd",
			lookup:  func(string) ([]net.IP, error) { return nil, nil },
			wantErr: true,
		},
		{
			name:    "ftp scheme rejected",
			url:     "ftp://example.com/file",
			lookup:  func(string) ([]net.IP, error) { return nil, nil },
			wantErr: true,
		},
		{
			name:    "empty host rejected",
			url:     "https:///path",
			lookup:  func(string) ([]net.IP, error) { return nil, nil },
			wantErr: true,
		},
		{
			name: "public ip accepted",
			url:  "https://example.com/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("93.184.216.34")}, nil
			},
			wantErr: false,
		},
		{
			name: "loopback rejected",
			url:  "https://localhost/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("127.0.0.1")}, nil
			},
			wantErr: true,
		},
		{
			name: "private 10.x rejected",
			url:  "https://internal.corp/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1")}, nil
			},
			wantErr: true,
		},
		{
			name: "private 192.168.x rejected",
			url:  "https://router.local/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("192.168.1.1")}, nil
			},
			wantErr: true,
		},
		{
			name: "link-local rejected",
			url:  "https://link.local/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("169.254.1.1")}, nil
			},
			wantErr: true,
		},
		{
			name: "dns rebinding mixed records rejected",
			url:  "https://rebind.example.com/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{
					net.ParseIP("93.184.216.34"), // public
					net.ParseIP("10.0.0.1"),      // private
				}, nil
			},
			wantErr: true,
		},
		{
			name: "no addresses resolved rejected",
			url:  "https://nx.example.com/page",
			lookup: func(string) ([]net.IP, error) {
				return nil, nil
			},
			wantErr:  true,
			wantSent: ErrDNSLookupFailed,
		},
		{
			name: "dns lookup error returns ErrDNSLookupFailed",
			url:  "https://transient.example.com/page",
			lookup: func(string) ([]net.IP, error) {
				return nil, &net.DNSError{Err: "i/o timeout", Name: "transient.example.com"}
			},
			wantErr:  true,
			wantSent: ErrDNSLookupFailed,
		},
		{
			name: "ipv6 public accepted",
			url:  "https://v6.example.com/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("2606:2800:220:1:248:1893:25c8:1946")}, nil
			},
			wantErr: false,
		},
		{
			name: "ipv6 loopback rejected",
			url:  "https://v6loop.example.com/page",
			lookup: func(string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("::1")}, nil
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookupAllIPs = tc.lookup
			err := ValidateFetchURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error for %q, got %v", tc.url, err)
			}
			if tc.wantSent != nil && err != nil {
				if !errors.Is(err, tc.wantSent) {
					t.Errorf("expected errors.Is(err, %v) for %q, got %v", tc.wantSent, tc.url, err)
				}
			}
		})
	}
}