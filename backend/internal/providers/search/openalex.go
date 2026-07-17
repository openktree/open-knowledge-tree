package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

const openAlexURL = "https://api.openalex.org/works"

// openAlexDefaultPerPage is the page size OpenAlex returns when the
// caller does not supply one. 20 is the upstream default and matches
// the pre-pagination behavior.
const openAlexDefaultPerPage = 20

// openAlexMaxPerPage caps the page size we forward to OpenAlex. The
// upstream hard-cap is 200; we enforce it here so a misbehaving
// caller can't ask for a huge page.
const openAlexMaxPerPage = 200

// OpenAlexSearchProvider implements SearchProvider against the OpenAlex
// Works API (https://api.openalex.org/works).
type OpenAlexSearchProvider struct {
	email      string
	httpClient *http.Client
}

func NewOpenAlexSearchProvider(email string) *OpenAlexSearchProvider {
	return &OpenAlexSearchProvider{
		email: email,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func NewOpenAlexSearchProviderFromConfig(openAlexCfg config.OpenAlexProviderConfig) *OpenAlexSearchProvider {
	return NewOpenAlexSearchProvider(openAlexCfg.Email)
}

type openAlexResponse struct {
	Meta    openAlexMeta  `json:"meta"`
	Results []openAlexWork `json:"results"`
}

// openAlexMeta carries the cursor pagination metadata OpenAlex
// returns at the top of every Works response. `Count` is the total
// number of works matching the query (independent of paging);
// `NextCursor` is the opaque cursor to pass as `cursor=` to fetch
// the next page. It is empty/null when the current page is the
// last one.
type openAlexMeta struct {
	Count      int64  `json:"count"`
	NextCursor string `json:"next_cursor"`
}

type openAlexWork struct {
	Title           string `json:"title"`
	ID              string `json:"id"`
	DOI             string `json:"doi"`
	PrimaryLocation *struct {
		LandingPageURL string `json:"landing_page_url"`
	} `json:"primary_location"`
	AbstractInvertedIndex map[string][]int `json:"abstract_inverted_index"`
	// PublicationDate is the ISO 8601 date string
	// OpenAlex ships on every Work record (e.g.
	// "2018-02-13"). Absent for ~10% of records (older
	// works, paratext). We parse it on the application
	// side so the SearchResult type can carry a
	// *time.Time uniformly.
	PublicationDate string `json:"publication_date"`
}

func (p *OpenAlexSearchProvider) Search(ctx context.Context, query string, opts SearchOptions) (SearchResponse, error) {
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = openAlexDefaultPerPage
	}
	if perPage > openAlexMaxPerPage {
		perPage = openAlexMaxPerPage
	}

	reqURL, err := url.Parse(openAlexURL)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("parsing openalex URL: %w", err)
	}

	q := reqURL.Query()
	q.Set("search", query)
	// Sort by relevance (OpenAlex's default full-text ranking across
	// title/abstract/metadata) so the most topically relevant works
	// surface first. The previous cited_by_count:desc sort returned
	// the most-cited papers matching any search term, which produced
	// high-impact-but-irrelevant results for topical queries.
	q.Set("sort", "relevance_score:desc")
	q.Set("per_page", strconv.Itoa(perPage))
	// OpenAlex cursor pagination: `cursor=*` selects the first
	// page; the response's meta.next_cursor is the opaque token
	// to pass back as `cursor=` for the next page. An empty
	// cursor on the caller side means "first page".
	cursor := opts.Cursor
	if cursor == "" {
		cursor = "*"
	}
	q.Set("cursor", cursor)
	if p.email != "" {
		q.Set("mailto", p.email)
	}
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("creating openalex request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("openalex request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return SearchResponse{}, fmt.Errorf("openalex returned status %d: %s", resp.StatusCode, string(b))
	}

	var result openAlexResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return SearchResponse{}, fmt.Errorf("decoding openalex response: %w", err)
	}

	results := make([]SearchResult, 0, len(result.Results))
	for _, w := range result.Results {
		link := w.ID
		if w.PrimaryLocation != nil && w.PrimaryLocation.LandingPageURL != "" {
			link = w.PrimaryLocation.LandingPageURL
		}

		results = append(results, SearchResult{
			Title:       w.Title,
			URL:         link,
			Snippet:     truncateSnippet(reconstructAbstract(w.AbstractInvertedIndex), 300),
			DOI:         stripDOIURLPrefix(w.DOI),
			OpenAlexID:  openAlexKeyFromID(w.ID),
			PublishedAt: parseOpenAlexDate(w.PublicationDate),
		})
	}

	return SearchResponse{
		Results:    results,
		Total:      result.Meta.Count,
		NextCursor: result.Meta.NextCursor,
	}, nil
}

// stripDOIURLPrefix reduces "https://doi.org/10.123/abc" to
// "10.123/abc" so consumers can store and compare bare DOIs.
// OpenAlex returns the full URL form in Work.doi; we keep the
// convention consistent with fetch.ClassifyURL so the worker's
// classifier and the search provider agree on the canonical
// DOI form.
func stripDOIURLPrefix(s string) string {
	if s == "" {
		return ""
	}
	for _, p := range []string{
		"https://doi.org/",
		"http://doi.org/",
		"https://dx.doi.org/",
		"http://dx.doi.org/",
	} {
		if len(s) > len(p) && s[:len(p)] == p {
			return s[len(p):]
		}
	}
	return s
}

// openAlexKeyFromID returns "W2144634347" from
// "https://openalex.org/W2144634347". OpenAlex always returns
// the full URL form in Work.id; we keep the bare key for
// comparison and storage.
func openAlexKeyFromID(s string) string {
	const prefix = "https://openalex.org/"
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// parseOpenAlexDate parses OpenAlex's "publication_date"
// field (ISO 8601 date, e.g. "2018-02-13") into a
// *time.Time. Returns nil when the field is empty or
// unparseable. The pointer return is what lets the
// SearchResult struct distinguish "no date" (nil) from
// "epoch" (non-nil pointing at time.Time{}), which is
// the same distinction the database column makes with
// NULL.
//
// OpenAlex occasionally returns partial dates
// (year-only "2018", year-month "2018-02"); we accept
// them and zero the missing components so a future
// cross-source date merger can still see the year.
func parseOpenAlexDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	// Date-only ISO 8601 is what the field promises.
	// time.Parse with the date-only layout is strict
	// about the format; we fall back to year-only and
	// year-month forms for the partial cases OpenAlex
	// occasionally returns.
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

func reconstructAbstract(inverted map[string][]int) string {
	if len(inverted) == 0 {
		return ""
	}

	type token struct {
		word     string
		position int
	}

	var tokens []token
	for word, positions := range inverted {
		for _, pos := range positions {
			tokens = append(tokens, token{word: word, position: pos})
		}
	}

	for i := 0; i < len(tokens); i++ {
		for j := i + 1; j < len(tokens); j++ {
			if tokens[i].position > tokens[j].position {
				tokens[i], tokens[j] = tokens[j], tokens[i]
			}
		}
	}

	var abstract string
	for _, t := range tokens {
		if len(abstract) > 0 {
			abstract += " "
		}
		abstract += t.word
	}

	return abstract
}

func truncateSnippet(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
