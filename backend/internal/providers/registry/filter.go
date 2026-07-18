package registry

// RelevanceFilter bundles every per-repository restriction the
// registry core applies when pulling decompositions. It is the
// single struct the workers build once per job and thread through
// Service.PullRelevantDecomposition so the Service can return only
// the elements relevant to this repository.
//
// The filter consolidates four axes that were previously scattered
// across the pull workers (resolveAllowedModels, GetRepositorySyncLevels,
// promptsetResolver.AcceptedHashes, NewInboundContextMapper):
//
//   - AllowedModels: the per-repo model whitelist (per-repo replaces
//     global; ["*"] = all, [] = none, nil = inherit client's global).
//   - AcceptedPromptsets: the promptset hashes a repo will admit on
//     pull. Empty/nil = accept all (the default-accept semantics that
//     preserve legacy behavior for deployments that haven't adopted
//     promptsets). A decomposition whose promptset_hash is empty is
//     treated as the default and always accepted.
//   - SyncLevel: the SyncLevelFilter that strips concept-level fields
//     when the repo's pull level is "facts". Nil = full "concepts" pull.
//   - ContextMapper: the inbound context mapper that translates
//     registry concept contexts to the repo's local vocabulary and
//     applies the unmapped_context_policy (skip | auto_add |
//     catch_all). Nil = import verbatim (the legacy behavior before
//     context mapping shipped).
//   - AutoAdd: the callback the mapper invokes when the policy is
//     auto_add and the registry label isn't already a local context.
//     The caller seeds a repository_contexts row so the import can
//     land. Nil = no auto-add (the concept is dropped instead).
type RelevanceFilter struct {
	AllowedModels      []string
	AcceptedPromptsets []string
	SyncLevel          *SyncLevelFilter
	ContextMapper      InboundContextMapper
	AutoAdd            func(string)
}

// InboundContextMapper is the minimal slice of the inbound context
// mapper the registry core needs. The concrete implementation lives
// in the tasks package (InboundContextMapper); this interface lets
// the registry package depend on the shape without importing tasks
// (which would create a cycle, since tasks imports registry).
type InboundContextMapper interface {
	// MapContext returns the local context label for a registry
	// context and whether the concept should be imported. When the
	// second return is false, the caller skips the concept (and any
	// link to it). autoAdd is invoked when the policy is auto_add.
	MapContext(registryContext string, autoAdd func(string)) (string, bool)
}

// AllowsModel reports whether a decomposition model id passes the
// per-repo model whitelist. Delegates to the package-level IsAllowed
// helper so ["*"], [], and explicit lists all behave the same as the
// existing import workers. A nil filter allows everything (the
// permissive default for a deployment that hasn't configured the
// restriction).
func (f *RelevanceFilter) AllowsModel(modelID string) bool {
	if f == nil {
		return true
	}
	if len(f.AllowedModels) == 0 {
		// An empty (non-nil) list means "allow none" per IsAllowed,
		// but a nil filter means "no restriction". Distinguish by
		// checking the slice header: nil filter → allow; non-nil
		// empty → IsAllowed returns false. This matches the existing
		// resolveAllowedModels which returns the fallback (which may
		// be empty) and IsAllowed's [] → false semantics.
		return IsAllowed(f.AllowedModels, modelID)
	}
	return IsAllowed(f.AllowedModels, modelID)
}

// AllowsPromptset reports whether a decomposition's promptset_hash is
// in the repo's accepted set. Empty AcceptedPromptsets = accept all
// (the default-accept semantics). A decomposition with an empty hash
// is treated as the default and always accepted — this preserves the
// legacy behavior when the registry server hasn't shipped
// promptset_hash on DecompRef yet.
func (f *RelevanceFilter) AllowsPromptset(hash string) bool {
	if f == nil {
		return true
	}
	if len(f.AcceptedPromptsets) == 0 {
		return true
	}
	if hash == "" {
		return true
	}
	for _, h := range f.AcceptedPromptsets {
		if h == hash {
			return true
		}
	}
	return false
}

// MapContext routes a registry context through the inbound mapper
// when one is configured; a nil mapper imports verbatim (the legacy
// behavior before context mapping shipped). The autoAdd callback is
// threaded through so the mapper can seed a repository_contexts row
// for the auto_add policy.
func (f *RelevanceFilter) MapContext(registryContext string) (string, bool) {
	if f == nil || f.ContextMapper == nil {
		return registryContext, true
	}
	return f.ContextMapper.MapContext(registryContext, f.AutoAdd)
}