// Command backfill-source-metadata parses the YAML frontmatter that
// download_dataset.py writes at the top of each uploaded markdown
// corpus file (title, source, author, category, published_at) and
// populates the matching columns on okt_repository.sources:
// parsed_title, parsed_sitename (from `source`), parsed_author, and
// published_at.
//
// The upload ingestion path (Source.UploadSource) writes the raw
// markdown — frontmatter included — into parsed_text but never harvests
// the frontmatter into the structured source columns, so for uploaded
// markdown those four columns are always NULL. This backfill closes
// that gap without re-ingesting: it scans parsed_text for the leading
// `---\n...\n---` block, extracts the quoted scalar after each known
// key, and UPDATEs the columns that are still NULL.
//
// The script is idempotent: by default it only touches rows where the
// target column IS NULL, so re-running after partial completion is
// safe and re-running after the columns are populated is a no-op.
// Pass --force to overwrite existing non-NULL values too (useful when
// the frontmatter was corrected after the first run).
//
// Scope:
//   - By default, all repositories in the target database. Pass
//     --repo=<slug-or-uuid> to scope to one repository.
//   - Only sources whose url starts with `upload://` and ends in `.md`
//     are considered. URL-fetched sources (http://, https://) already
//     go through the full parser pipeline, and non-markdown uploads
//     have no frontmatter to harvest.
//   - Only sources whose parsed_text begins with `---` are touched,
//     so markdown uploads that happen to have no frontmatter are
//     skipped silently.
//
// Safety:
//   - --dry-run (default) prints what would change and writes nothing.
//     Pass --apply to commit the updates.
//   - Refuses to run against the e2e test DB (port 5433) — the test
//     harness owns that.
//
// Usage:
//
//	# Dry run (default) — prints the plan, writes nothing
//	go run ./scripts/backfill-source-metadata
//	go run ./scripts/backfill-source-metadata --repo=multihoprag
//
//	# Apply
//	go run ./scripts/backfill-source-metadata --apply
//	go run ./scripts/backfill-source-metadata --repo=multihoprag --apply
//
// Connects to the dev Postgres (port 5432) by default. Override with
// OKT_DATABASE_URL (same env var the other scripts use).
package main

import (
	"context"
	"flag"
	"fmt"
	"html"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()

	var (
		repoIdent string
		apply     bool
		force     bool
	)
	flag.StringVar(&repoIdent, "repo", "", "scope to one repository (slug or UUID); empty = all repos")
	flag.BoolVar(&apply, "apply", false, "commit the updates (default is dry-run)")
	flag.BoolVar(&force, "force", false, "overwrite non-NULL columns too (default only fills NULLs)")
	flag.Parse()

	dbURL := os.Getenv("OKT_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_dev@localhost:5432/okt?sslmode=disable"
	}
	if err := guardTestDB(dbURL); err != nil {
		fmt.Fprintln(os.Stderr, "refusing to run:", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connecting to postgres: %v", err)
	}
	defer pool.Close()

	repoID, err := resolveRepo(ctx, pool, repoIdent)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mode := "dry-run"
	if apply {
		mode = "apply"
	}
	if force {
		mode += " (force overwrite)"
	}
	scope := "all repositories"
	if repoID != nil {
		scope = fmt.Sprintf("repository %s", *repoID)
	}
	log.Printf("backfill-source-metadata: %s, scope=%s", mode, scope)

	plan, err := buildPlan(ctx, pool, repoID, force)
	if err != nil {
		log.Fatalf("building plan: %v", err)
	}
	printPlan(plan)

	if !apply {
		log.Printf("dry-run complete. Re-run with --apply to commit these updates.")
		return
	}

	stats, err := applyPlan(ctx, pool, plan)
	if err != nil {
		log.Fatalf("applying plan: %v", err)
	}
	log.Printf("applied: %d sources updated, %d fields written",
		stats.sourcesUpdated, stats.fieldsWritten)
}

// planRow is one source + the four fields we propose to write.
type planRow struct {
	sourceID      string
	url           string
	parsedTitle   *string
	parsedSitename *string
	parsedAuthor  *string
	publishedAt   *time.Time
}

type plan struct {
	rows []planRow
	// Counts for the report.
	totalScanned   int
	skippedNoFM    int // parsed_text didn't start with ---
	skippedNoKeys  int // frontmatter present but no harvestable keys
	skippedAlready int // all four target columns already non-NULL (and --force not set)
}

type applyStats struct {
	sourcesUpdated int
	fieldsWritten  int
}

// frontmatter regexes — quoted scalar form: key: "value"
// The corpus is uniformly quoted, so we handle the common case and
// fall through (no match) for any oddball.
var (
	reTitle      = regexp.MustCompile(`(?m)^title:\s*"(.+)"\s*$`)
	reSource     = regexp.MustCompile(`(?m)^source:\s*"(.+)"\s*$`)
	reAuthor     = regexp.MustCompile(`(?m)^author:\s*"(.+)"\s*$`)
	rePublished  = regexp.MustCompile(`(?m)^published_at:\s*"(.+)"\s*$`)
	// A unicode escape that shows up in some downloaded titles
	// (e.g. \u2019 -> '). We unescape a small set so the stored title
	// is human-readable. PostgreSQL's regex doesn't replace these, so
	// we do it in Go after extraction.
	reUnicodeEsc = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)
)

// guardTestDB refuses to run against the e2e test DB. Mirrors the
// guard in scripts/reset-repo and scripts/diagnose-sources.
func guardTestDB(dbURL string) error {
	if strings.Contains(dbURL, ":5433") {
		return fmt.Errorf("OKT_DATABASE_URL points at the e2e test DB (port 5433); use the dev DB on port 5432")
	}
	return nil
}

// resolveRepo turns the --repo flag (slug or UUID) into a repo UUID.
// Empty input returns nil (meaning: all repos).
func resolveRepo(ctx context.Context, pool *pgxpool.Pool, ident string) (*string, error) {
	if ident == "" {
		return nil, nil
	}
	var id string
	// Try UUID first.
	err := pool.QueryRow(ctx, `
		SELECT id FROM okt_system.repositories
		WHERE id::text = $1 OR lower(slug) = lower($1)`,
		ident,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("repository %q not found: %w", ident, err)
	}
	return &id, nil
}

// buildPlan scans the candidate sources, parses frontmatter from
// parsed_text, and returns the proposed updates. When force=false,
// rows whose four target columns are all non-NULL are skipped.
func buildPlan(ctx context.Context, pool *pgxpool.Pool, repoID *string, force bool) (*plan, error) {
	q := `
		SELECT s.id::text, s.url, s.parsed_text,
		       s.parsed_title, s.parsed_sitename,
		       s.parsed_author, s.published_at
		FROM okt_repository.sources s
		WHERE s.url LIKE 'upload://%.md'
	`
	args := []any{}
	if repoID != nil {
		q += " AND s.repository_id = $1"
		args = append(args, *repoID)
	}
	q += " ORDER BY s.url"

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	p := &plan{}
	for rows.Next() {
		p.totalScanned++
		var (
			id, url, parsedText string
			curTitle, curSite, curAuthor *string
			curPub                       *time.Time
		)
		if err := rows.Scan(&id, &url, &parsedText, &curTitle, &curSite, &curAuthor, &curPub); err != nil {
			return nil, err
		}

		// Frontmatter detection: must start with --- on its own line.
		if !strings.HasPrefix(parsedText, "---\n") {
			p.skippedNoFM++
			continue
		}
		// Extract the frontmatter block (between the first and
		// second --- lines). regexp with (?m)^...$ anchors per-line.
		fmEnd := strings.Index(parsedText[4:], "\n---\n")
		if fmEnd < 0 {
			p.skippedNoFM++
			continue
		}
		fm := parsedText[4 : 4+fmEnd]

		title := extractQuoted(fm, reTitle)
		site := extractQuoted(fm, reSource)
		author := extractQuoted(fm, reAuthor)
		pub := parsePublishedAt(extractQuoted(fm, rePublished))

		// Skip if nothing to harvest.
		if title == "" && site == "" && author == "" && pub == nil {
			p.skippedNoKeys++
			continue
		}

		// Decide which fields to write. When not forcing, skip a
		// field that is already non-NULL (so we don't clobber a value
		// set by some other path). When all four are already set and
		// we're not forcing, skip the row entirely.
		var (
			wTitle *string
			wSite  *string
			wAuth  *string
			wPub   *time.Time
		)
		maybeSet := func(newV string, curV *string) *string {
			if newV == "" {
				return nil
			}
			if !force && curV != nil && *curV != "" {
				return nil
			}
			v := newV
			return &v
		}
		wTitle = maybeSet(title, curTitle)
		wSite = maybeSet(site, curSite)
		wAuth = maybeSet(author, curAuthor)
		if pub != nil && (force || curPub == nil) {
			wPub = pub
		}
		if wTitle == nil && wSite == nil && wAuth == nil && wPub == nil {
			p.skippedAlready++
			continue
		}

		p.rows = append(p.rows, planRow{
			sourceID:       id,
			url:            url,
			parsedTitle:    wTitle,
			parsedSitename: wSite,
			parsedAuthor:   wAuth,
			publishedAt:    wPub,
		})
	}
	return p, rows.Err()
}

// extractQuoted pulls the first capture group of re out of fm, or "".
// Also unescapes \uXXXX escapes the downloader emits and HTML
// numeric entities (e.g. &#039; -> ') that appear in some titles.
func extractQuoted(fm string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(fm)
	if m == nil {
		return ""
	}
	s := unescapeUnicode(m[1])
	// html.UnescapeString handles &#039; &#39; &#x27; &amp; &quot; etc.
	// Safe to apply to all frontmatter scalars — they're plain text,
	// never HTML.
	return html.UnescapeString(s)
}

func unescapeUnicode(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	return reUnicodeEsc.ReplaceAllStringFunc(s, func(match string) string {
		var code int
		fmt.Sscanf(match, `\u%04x`, &code)
		return string(rune(code))
	})
}

// parsePublishedAt accepts the ISO-8601 strings the downloader writes
// (e.g. 2023-09-28T12:00:00+00:00) and returns a time.Time. Returns
// nil on any parse failure (the row is simply skipped for that field).
func parsePublishedAt(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	// Try a few layouts to be tolerant of edge cases.
	layouts := []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		t, err := time.Parse(l, raw)
		if err == nil {
			// Truncate to date — the column type is `date`.
			d := t.UTC().Truncate(24 * time.Hour)
			return &d
		}
	}
	return nil
}

func printPlan(p *plan) {
	fmt.Printf("\nScanned %d upload://*.md sources:\n", p.totalScanned)
	fmt.Printf("  skipped (no frontmatter):     %d\n", p.skippedNoFM)
	fmt.Printf("  skipped (no harvestable keys): %d\n", p.skippedNoKeys)
	fmt.Printf("  skipped (already populated):   %d\n", p.skippedAlready)
	fmt.Printf("  planned updates:               %d\n", len(p.rows))

	// Sample 5 rows so the operator can eyeball the extraction.
	fmt.Println("\nSample (first 5 planned updates):")
	limit := 5
	if len(p.rows) < limit {
		limit = len(p.rows)
	}
	for i := 0; i < limit; i++ {
		r := p.rows[i]
		fmt.Printf("  %s\n", r.url)
		if r.parsedTitle != nil {
			fmt.Printf("    parsed_title:    %q\n", *r.parsedTitle)
		}
		if r.parsedSitename != nil {
			fmt.Printf("    parsed_sitename: %q\n", *r.parsedSitename)
		}
		if r.parsedAuthor != nil {
			fmt.Printf("    parsed_author:   %q\n", *r.parsedAuthor)
		}
		if r.publishedAt != nil {
			fmt.Printf("    published_at:    %s\n", r.publishedAt.Format("2006-01-02"))
		}
	}
	if len(p.rows) > 5 {
		fmt.Printf("  ... and %d more\n", len(p.rows)-5)
	}
	fmt.Println()
}

// applyPlan runs one UPDATE per planned row, only SETting the fields
// that are non-nil in the planRow. Returns the count of updated rows
// and the total field count written.
func applyPlan(ctx context.Context, pool *pgxpool.Pool, p *plan) (*applyStats, error) {
	st := &applyStats{}
	for _, r := range p.rows {
		set := []string{}
		args := []any{}
		n := 1
		if r.parsedTitle != nil {
			set = append(set, fmt.Sprintf("parsed_title = $%d", n))
			args = append(args, *r.parsedTitle)
			n++
			st.fieldsWritten++
		}
		if r.parsedSitename != nil {
			set = append(set, fmt.Sprintf("parsed_sitename = $%d", n))
			args = append(args, *r.parsedSitename)
			n++
			st.fieldsWritten++
		}
		if r.parsedAuthor != nil {
			set = append(set, fmt.Sprintf("parsed_author = $%d", n))
			args = append(args, *r.parsedAuthor)
			n++
			st.fieldsWritten++
		}
		if r.publishedAt != nil {
			set = append(set, fmt.Sprintf("published_at = $%d", n))
			args = append(args, *r.publishedAt)
			n++
			st.fieldsWritten++
		}
		if len(set) == 0 {
			continue
		}
		q := fmt.Sprintf(
			"UPDATE okt_repository.sources SET %s WHERE id = $%d",
			strings.Join(set, ", "), n,
		)
		args = append(args, r.sourceID)
		ct, err := pool.Exec(ctx, q, args...)
		if err != nil {
			return st, fmt.Errorf("updating %s: %w", r.sourceID, err)
		}
		if ct.RowsAffected() == 1 {
			st.sourcesUpdated++
		}
	}
	return st, nil
}