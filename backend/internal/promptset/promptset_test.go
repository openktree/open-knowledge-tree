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

// TestDefaultRegistryHashStable asserts the built-in promptset's
// REGISTRY-compatibility hash is deterministic across runs and
// matches a recomputation. Pins the value so a prompt edit that
// affects the 4 shared phases (fact/image-fact/concept/refinement)
// surfaces as a test failure — updating the pinned value is a
// deliberate "the shared philosophy changed" decision.
func TestDefaultRegistryHashStable(t *testing.T) {
	t.Parallel()
	if Default.RegistryHash == "" {
		t.Fatal("Default.RegistryHash is empty; init did not run")
	}
	if DefaultRegistryHash == "" {
		t.Fatal("DefaultRegistryHash is empty; init did not run")
	}
	if Default.RegistryHash != DefaultRegistryHash {
		t.Fatalf("Default.RegistryHash=%q DefaultRegistryHash=%q", Default.RegistryHash, DefaultRegistryHash)
	}
	if got := RegistryHashPromptset(Default); got != DefaultRegistryHash {
		t.Fatalf("RegistryHashPromptset(Default)=%q want %q", got, DefaultRegistryHash)
	}
	// DefaultRegistryHashes is seeded with DefaultRegistryHash.
	if len(DefaultRegistryHashes) == 0 || DefaultRegistryHashes[0] != DefaultRegistryHash {
		t.Fatalf("DefaultRegistryHashes=%v want [%q]", DefaultRegistryHashes, DefaultRegistryHash)
	}
}

// TestRegistryHashIgnoresLocalPhases asserts that changing ONLY the
// local phases (synthesis, image_picker, summarization, posture) does
// NOT change the RegistryHash — two promptsets that differ only in
// local phases are registry-compatible and can exchange
// decompositions. The full catalog Hash MUST change (the promptset
// is still a distinct catalog entry).
func TestRegistryHashIgnoresLocalPhases(t *testing.T) {
	t.Parallel()
	clone := Default
	// Mutate every local-only phase.
	clone.Synthesis = clone.Synthesis + " (tweaked)"
	clone.ImagePicker = clone.ImagePicker + " (tweaked)"
	clone.Summarization = clone.Summarization + " (tweaked)"
	clone.Posture = clone.Posture + " (tweaked)"
	clone = clone.WithHash()
	if clone.RegistryHash != DefaultRegistryHash {
		t.Fatalf("local-only mutation changed RegistryHash: got %q want %q", clone.RegistryHash, DefaultRegistryHash)
	}
	if clone.Hash == DefaultHash {
		t.Fatal("local-only mutation did not change catalog Hash; expected a new catalog entry")
	}
}

// TestRegistryHashChangesOnSharedPhaseMutation asserts that changing
// any of the 4 shared phases (fact/image-fact/concept/refinement)
// DOES change the RegistryHash — a different fact-extraction prompt
// is a different philosophy and must NOT collapse to the default
// compatibility class.
func TestRegistryHashChangesOnSharedPhaseMutation(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*Promptset){
		func(p *Promptset) { p.FactExtraction = p.FactExtraction + " (tweaked)" },
		func(p *Promptset) { p.ImageFactExtraction = p.ImageFactExtraction + " (tweaked)" },
		func(p *Promptset) { p.ConceptExtraction = p.ConceptExtraction + " (tweaked)" },
		func(p *Promptset) { p.Refinement = p.Refinement + " (tweaked)" },
	} {
		clone := Default
		mutate(&clone)
		clone = clone.WithHash()
		if clone.RegistryHash == DefaultRegistryHash {
			t.Fatalf("shared-phase mutation did not change RegistryHash; still %q", DefaultRegistryHash)
		}
	}
}

// TestRegistryHashCollapsesToDefault asserts that a custom promptset
// whose 4 shared phase strings equal the built-in's collapses to
// DefaultRegistryHash (not some other hash of the same 4 strings) —
// so the operator who only customized the summarizer still
// contributes to / pulls from the default compatibility class.
func TestRegistryHashCollapsesToDefault(t *testing.T) {
	t.Parallel()
	custom := Default
	custom.Name = "my custom promptset"
	custom.Source = CustomSource
	custom.Summarization = "totally different summarizer prompt"
	custom.Synthesis = "totally different synthesis prompt"
	custom.ImagePicker = "totally different image picker prompt"
	custom.Posture = "totally different posture prompt"
	custom = custom.WithHash()
	if custom.RegistryHash != DefaultRegistryHash {
		t.Fatalf("custom promptset with default shared phases did not collapse to DefaultRegistryHash: got %q want %q",
			custom.RegistryHash, DefaultRegistryHash)
	}
	// And it is NOT the same catalog entry (the full Hash differs).
	if custom.Hash == DefaultHash {
		t.Fatal("custom promptset with different local phases collapsed the catalog Hash; expected a distinct row")
	}
}