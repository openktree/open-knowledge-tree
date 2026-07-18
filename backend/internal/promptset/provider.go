package promptset

// Provider is the strategy interface for resolving a promptset hash
// to a Promptset and enumerating the available promptsets. Mirrors
// the search.SearchProvider / fetch.ResolutionProvider pattern: a
// provider is transport-agnostic and the Resolver chains several.
//
// Two implementations ship:
//   - BuiltinProvider returns the single built-in Promptset.
//   - DBProvider reads user-defined promptsets from
//     okt_system.promptsets via *store.Queries.
//
// The Resolver (see resolver.go) consults built-in first, then DB,
// so a custom promptset with the same phase strings as the built-in
// resolves to the built-in (same hash = same philosophy).
type Provider interface {
	// Get returns the Promptset for the given hash, or ok=false when
	// this provider does not know it. An empty hash is treated as
	// "the built-in default" by BuiltinProvider and as "not found"
	// by DBProvider; the Resolver normalises the empty case.
	Get(hash string) (Promptset, bool)

	// List returns every promptset this provider knows. The built-in
	// provider returns a one-element slice (Default); the DB provider
	// returns every row visible to the caller (the caller decides
	// visibility — List is the raw catalog).
	List() []Promptset
}

// BuiltinProvider is the Provider that returns only the built-in
// Promptset. Constructed once at wiring time; Get(DefaultHash)
// always succeeds and Get("") also returns Default (the empty hash
// is the "inherit global default" sentinel).
type BuiltinProvider struct{}

// NewBuiltinProvider returns a BuiltinProvider. It carries no state;
// the constructor exists so callers read "promptset.NewBuiltinProvider()"
// rather than the bare struct literal, matching the other provider
// packages' conventions.
func NewBuiltinProvider() BuiltinProvider { return BuiltinProvider{} }

// Get returns the built-in Promptset when hash is the built-in hash
// or the empty string (the "inherit global default" sentinel), and
// ok=false otherwise.
func (BuiltinProvider) Get(hash string) (Promptset, bool) {
	if hash == "" || hash == DefaultHash {
		return Default, true
	}
	return Promptset{}, false
}

// List returns a one-element slice containing the built-in Promptset.
func (BuiltinProvider) List() []Promptset { return []Promptset{Default} }