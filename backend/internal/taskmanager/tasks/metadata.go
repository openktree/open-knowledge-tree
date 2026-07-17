package tasks

import "encoding/json"

// JobMetadata is the JSON shape written to River's job `metadata`
// column at enqueue time so the per-repo tasks endpoint can filter
// jobs by repository (and optionally by source) without parsing the
// job's encoded_args. River supports metadata filtering via
// JobListParams.Metadata(jsonFragment), which performs a JSONB
// containment check — so a partial metadata object is enough to
// match.
//
// Every job the application enqueues carries at least `repo_id`.
// Jobs tied to a specific source (retrieve_source,
// source_decomposition, embed_facts) additionally carry `source_id`.
// Repo-wide jobs (deduplicate_facts, cleanup_facts) carry only
// `repo_id`. The fact_catchup periodic job carries neither and is
// not expected to appear in any per-repo listing.
type JobMetadata struct {
	RepositoryID string `json:"repo_id,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	ReportID     string `json:"report_id,omitempty"`
}

// MarshalMetadata serializes m into the []byte River's InsertOpts
// expects, or nil when m is the zero value (so jobs that carry no
// repo/source/report metadata — e.g. fact_catchup — keep a NULL
// metadata column instead of an empty `{}` blob).
func MarshalMetadata(m JobMetadata) []byte {
	if m.RepositoryID == "" && m.SourceID == "" && m.ReportID == "" {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}