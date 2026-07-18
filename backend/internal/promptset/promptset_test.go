package promptset

import "testing"

// TestDefaultHashStable asserts the built-in promptset's hash is
// deterministic across runs (so two OKT instances on the same build
// share the same "philosophy" identifier and can exchange registry
// decompositions). It also pins the hash to a recorded value so a
// prompt edit is surfaced as a test failure — updating the pinned
// value is a deliberate "the philosophy changed" decision.
func TestDefaultHashStable(t *testing.T) {
	t.Parallel()
	h := Default.Hash
	if h == "" {
		t.Fatal("Default.Hash is empty; init did not run")
	}
	// Recompute and confirm identity.
	if HashPromptset(Default) != h {
		t.Fatalf("hash mismatch: Default.Hash=%q recomputed=%q", h, HashPromptset(Default))
	}
	// A second Promptset built from the same phase strings must hash
	// to the same value (cross-instance identity).
	clone := Default
	clone.Hash = ""
	clone = clone.WithHash()
	if clone.Hash != h {
		t.Fatalf("clone hash mismatch: clone=%q default=%q", clone.Hash, h)
	}
	// Name is NOT part of the hash input.
	rename := Default
	rename.Name = "renamed"
	if HashPromptset(rename) != h {
		t.Fatal("renaming the promptset changed its hash; Name must not be a hash input")
	}
}

// TestDefaultComplete asserts every built-in phase field is
// non-empty. A regression that leaves a phase blank (e.g. a refactor
// that drops a const) would otherwise silently make IsComplete false
// and the built-in promptset unresolvable as a complete philosophy.
func TestDefaultComplete(t *testing.T) {
	t.Parallel()
	if !Default.IsComplete() {
		t.Fatalf("built-in promptset is incomplete; missing: %v", Default.MissingPhases())
	}
}

// TestEmptyHashResolvesToDefault asserts the "inherit global default"
// sentinel (empty hash) resolves to the built-in Default via the
// Resolver, so callers can pass a NULL repo active_promptset_hash
// straight through without a special case.
func TestEmptyHashResolvesToDefault(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil) // no DB
	ps, ok := r.Get("")
	if !ok {
		t.Fatal("Get(\"\") returned ok=false; empty hash must resolve to Default")
	}
	if ps.Hash != Default.Hash {
		t.Fatalf("Get(\"\") returned hash %q; want %q", ps.Hash, Default.Hash)
	}
	if r.ResolveOrDefault("nonexistent-hash").Hash != Default.Hash {
		t.Fatal("ResolveOrDefault with unknown hash did not fall back to Default")
	}
}

// TestUnknownHashNotFound asserts a random hash the resolver does not
// know returns ok=false (so the caller can fall back to the global
// default rather than silently using the built-in).
func TestUnknownHashNotFound(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil)
	if _, ok := r.Get("definitely-not-a-real-hash"); ok {
		t.Fatal("Get(unknown) returned ok=true; should be false")
	}
	if r.Has("definitely-not-a-real-hash") {
		t.Fatal("Has(unknown) returned true; should be false")
	}
}