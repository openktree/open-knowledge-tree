package registry

import "testing"

// TestAllowsPromptset_DefaultAccepted verifies the always-accepted
// list (DefaultAccepted) admits its members even when
// AcceptedPromptsets is non-empty and does NOT include them. This is
// the "built-in philosophy is always pullable" rule: a repo that has
// configured a custom active promptset still receives
// decompositions tagged with the default registry hash.
func TestAllowsPromptset_DefaultAccepted(t *testing.T) {
	t.Parallel()
	defaultHash := "default-registry-hash"
	customHash := "custom-registry-hash"
	f := &RelevanceFilter{
		AcceptedPromptsets: []string{customHash},
		DefaultAccepted:    []string{defaultHash},
	}
	if !f.AllowsPromptset(defaultHash) {
		t.Errorf("expected default hash %q to be accepted via DefaultAccepted", defaultHash)
	}
	if !f.AllowsPromptset(customHash) {
		t.Errorf("expected accepted hash %q to be accepted", customHash)
	}
	if f.AllowsPromptset("unknown") {
		t.Errorf("expected unknown hash to be rejected")
	}
}

// TestAllowsPromptset_EmptyHashAlwaysAccepted verifies an empty
// promptset_hash (a registry server that hasn't shipped the field)
// is always accepted — the legacy permissive behavior.
func TestAllowsPromptset_EmptyHashAlwaysAccepted(t *testing.T) {
	t.Parallel()
	f := &RelevanceFilter{
		AcceptedPromptsets: []string{"only-this"},
		DefaultAccepted:    []string{"default"},
	}
	if !f.AllowsPromptset("") {
		t.Errorf("expected empty hash to be accepted (legacy permissive behavior)")
	}
}

// TestAllowsPromptset_NilFilterAllowsAll verifies a nil filter is
// permissive (the default for a deployment that hasn't configured
// the restriction).
func TestAllowsPromptset_NilFilterAllowsAll(t *testing.T) {
	t.Parallel()
	var f *RelevanceFilter
	if !f.AllowsPromptset("anything") {
		t.Errorf("expected nil filter to accept everything")
	}
}

// TestAllowsPromptset_EmptyAcceptedAllowsAll verifies an empty (not
// nil) AcceptedPromptsets with an empty DefaultAccepted still admits
// every hash — the default-accept semantics that preserve legacy
// behavior for deployments that haven't adopted promptsets.
func TestAllowsPromptset_EmptyAcceptedAllowsAll(t *testing.T) {
	t.Parallel()
	f := &RelevanceFilter{
		AcceptedPromptsets: nil,
		DefaultAccepted:    nil,
	}
	if !f.AllowsPromptset("any-registry-hash") {
		t.Errorf("expected empty Accepted/Default to accept all (default-accept semantics)")
	}
}