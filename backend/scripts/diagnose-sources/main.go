// Command diagnose-sources prints a read-only classification of source
// retrieval health: how many sources are in each status, what error
// modes dominate the failed rows, and — most importantly — which
// "silent failures" (rows stamped fetched/processed whose parsed_text
// is a known boilerplate / captcha / cookies-disabled / challenge
// page) are hiding in the success bucket.
//
// It is the operator-facing companion to the boilerplate guard in
// internal/providers/fetch/resolution.go. Run it after deploying a
// guard expansion to confirm the silent-failure count dropped, or
// any time an operator wants to triage "why is this source's title
// 'Validate User'?".
//
// The tool is strictly read-only: it runs SELECTs against
// okt_repository.sources and prints a report. It never writes.
//
// Usage:
//
//	go run ./scripts/diagnose-sources
//
// Connects to the dev Postgres (port 5432) by default. Override with
// OKT_DATABASE_URL. Refuses to run against the e2e test DB (port
// 5433) for the same reason reset-repo does: the test harness drops
// schemas, and pointing a diagnostic tool at the test DB is always a
// mistake.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()

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

	printStatusOverview(ctx, pool)
	printPerProviderFailures(ctx, pool)
	printFailureTypeModes(ctx, pool)
	printSilentFailures(ctx, pool)
	printTopFailingHosts(ctx, pool)
}

// guardTestDB refuses to run against the e2e test DB. Mirrors the
// guard in scripts/reset-repo. The test harness drops all schemas,
// so pointing any operator tool at port 5433 is a mistake even for
// a read-only one (the report would be empty or misleading).
func guardTestDB(dbURL string) error {
	if strings.Contains(dbURL, ":5433") {
		return fmt.Errorf("OKT_DATABASE_URL points at the e2e test DB (port 5433); use the dev DB on port 5432")
	}
	return nil
}

func printStatusOverview(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("=== Status overview ===")
	rows, err := pool.Query(ctx, `
		SELECT status, parse_status, COUNT(*) AS n
		FROM okt_repository.sources
		GROUP BY 1, 2
		ORDER BY n DESC;`)
	if err != nil {
		log.Printf("status overview: %v", err)
		return
	}
	defer rows.Close()
	fmt.Printf("%-12s %-12s %6s\n", "status", "parse_status", "n")
	for rows.Next() {
		var status, parseStatus string
		var n int
		if err := rows.Scan(&status, &parseStatus, &n); err != nil {
			log.Printf("status overview scan: %v", err)
			continue
		}
		if parseStatus == "" {
			parseStatus = "(null)"
		}
		fmt.Printf("%-12s %-12s %6d\n", status, parseStatus, n)
	}
	fmt.Println()
}

func printPerProviderFailures(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("=== Per-provider failures (one row per failed attempt; a failed source can contribute to multiple providers) ===")
	rows, err := pool.Query(ctx, `
		WITH attempts AS (
			SELECT
				s.id,
				a->>'provider' AS provider,
				a->>'error'    AS err,
				(a->>'success')::boolean AS ok
			FROM okt_repository.sources s,
			     jsonb_array_elements(s.fetch_attempts) AS a
			WHERE s.status = 'failed' AND s.fetch_attempts IS NOT NULL
		)
		SELECT
			provider,
			CASE
				WHEN err ILIKE '%upstream returned status 403%' THEN 'tls_fetch_403'
				WHEN err ILIKE '%upstream returned status 401%' THEN 'tls_fetch_401'
				WHEN err ILIKE '%upstream returned status 404%' THEN 'tls_fetch_404'
				WHEN err ILIKE '%upstream returned status 429%' THEN 'tls_fetch_429'
				WHEN err ILIKE '%upstream returned status 5%' THEN 'tls_fetch_5xx'
				WHEN err ILIKE '%extracted content below minimum length%' THEN 'insufficient_content'
				WHEN err ILIKE '%context deadline exceeded%' THEN 'timeout'
				WHEN err ILIKE '%i/o timeout%' OR err ILIKE '%dial tcp%lookup%' THEN 'dns_timeout'
				WHEN err ILIKE '%failed to verify certificate%' OR err ILIKE '%x509%' THEN 'tls_cert_invalid'
				WHEN err ILIKE '%stream error%' OR err ILIKE '%INTERNAL_ERROR%' THEN 'http2_stream'
				WHEN err ILIKE '%response body exceeds max bytes%' THEN 'body_too_large'
				WHEN err ILIKE '%stopped after 10 redirects%' THEN 'redirect_loop'
				WHEN err ILIKE '%non-HTML content type%pdf%' THEN 'flares_pdf_skip'
				WHEN err ILIKE '%OA location returned status 403%' THEN 'unpaywall_oa_403'
				WHEN err ILIKE '%no open-access location%' THEN 'unpaywall_closed'
				WHEN err ILIKE '%sidecar returned status 500%' THEN 'flares_500'
				WHEN err ILIKE '%unsafe URL rejected%' THEN 'ssrf_rejected'
				WHEN err IS NULL OR err = '' THEN 'no_error_field'
				ELSE 'other'
			END AS err_mode,
			COUNT(*) AS n
		FROM attempts
		WHERE ok = false
		GROUP BY 1, 2
		ORDER BY provider, n DESC;`)
	if err != nil {
		log.Printf("per-provider failures: %v", err)
		return
	}
	defer rows.Close()
	fmt.Printf("%-14s %-26s %6s\n", "provider", "err_mode", "n")
	for rows.Next() {
		var provider, mode string
		var n int
		if err := rows.Scan(&provider, &mode, &n); err != nil {
			log.Printf("per-provider scan: %v", err)
			continue
		}
		fmt.Printf("%-14s %-26s %6d\n", provider, mode, n)
	}
	fmt.Println()
}

func printFailureTypeModes(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("=== Failure-type modes (one row per failed source, classified by combined error signature) ===")
	rows, err := pool.Query(ctx, `
		SELECT
			CASE
				WHEN error = 'silent boilerplate detected on backfill' THEN 'AA_backfilled_silent_failure'
				WHEN error LIKE '%status 403%' AND error LIKE '%context deadline exceeded%' AND error LIKE '%flaresolverr%' THEN 'A_flare_timeout_403_chain'
				WHEN error LIKE '%extracted content below minimum length%' AND error NOT LIKE '%status 4%' AND error NOT LIKE '%context deadline%' THEN 'B_insufficient_content_all_tiers'
				WHEN error LIKE '%status 403%' AND error LIKE '%flaresolverr%non-HTML content type%pdf%' THEN 'C_pdf_403_flares_pdf_skip'
				WHEN error LIKE '%status 403%' AND error LIKE '%flaresolverr%extracted content%' THEN 'D_403_challenge_pages'
				WHEN error LIKE '%status 403%' AND error LIKE '%context deadline exceeded%flaresolverr%' THEN 'E_403_flares_timeout'
				WHEN error LIKE '%status 429%' THEN 'F_rate_limited'
				WHEN error LIKE '%status 401%' THEN 'G_unauthorized'
				WHEN error LIKE '%status 404%' THEN 'H_not_found'
				WHEN error LIKE '%context deadline exceeded%' AND error NOT LIKE '%status 4%' AND error NOT LIKE '%extracted content%' THEN 'I_timeout_network'
				WHEN error LIKE '%tls: failed to verify certificate%' OR error LIKE '%x509%' THEN 'J_tls_cert_invalid'
				WHEN error LIKE '%unsafe URL rejected by SSRF%' THEN 'K_ssrf_dns_fail'
				WHEN error LIKE '%stream error%' OR error LIKE '%INTERNAL_ERROR%' THEN 'L_http2_stream_error'
				WHEN error LIKE '%response body exceeds max bytes%' THEN 'M_body_too_large'
				WHEN error LIKE '%unpaywall: no open-access location%' THEN 'N_unpaywall_closed'
				WHEN error LIKE '%OA location returned status 403%' THEN 'O_unpaywall_oa_403'
				WHEN error LIKE '%non-HTML content type%pdf%' THEN 'P_pdf_via_flares_skipped'
				WHEN error LIKE '%extracted content below minimum length%' THEN 'Q_insufficient_content_partial'
				WHEN error LIKE '%status 403%' THEN 'R_403_other'
				WHEN error LIKE '%context deadline exceeded%' THEN 'S_timeout_other'
				WHEN error LIKE '%dial tcp%lookup%i/o timeout%' THEN 'T_dns_timeout'
				WHEN error LIKE '%flaresolverr: sidecar returned status 500%' THEN 'U_flares_500'
				WHEN error LIKE '%stopped after 10 redirects%' THEN 'V_redirect_loop'
				ELSE 'Z_other'
			END AS mode,
			COUNT(*) AS n
		FROM okt_repository.sources
		WHERE status = 'failed'
		GROUP BY 1
		ORDER BY n DESC;`)
	if err != nil {
		log.Printf("failure type modes: %v", err)
		return
	}
	defer rows.Close()
	fmt.Printf("%-36s %6s\n", "mode", "n")
	for rows.Next() {
		var mode string
		var n int
		if err := rows.Scan(&mode, &n); err != nil {
			log.Printf("failure type scan: %v", err)
			continue
		}
		fmt.Printf("%-36s %6d\n", mode, n)
	}
	fmt.Println()
}

func printSilentFailures(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("=== Silent failures (status=fetched/processed, parsed_text matches a known boilerplate signature) ===")
	rows, err := pool.Query(ctx, `
		SELECT
			CASE
				WHEN parsed_text ILIKE '%please help us confirm that you are not a robot%'
					OR parsed_text ILIKE '%could not validate captcha%' THEN 'oup_validate_user_captcha'
				WHEN parsed_text ILIKE '%making sure you''re not a bot%' THEN 'bot_challenge_js'
				WHEN parsed_text ILIKE '%dear visitor%to continue browsing%fight cybercrime%' THEN 'captcha_landing'
				WHEN parsed_text ILIKE '%site protection%verifying your request%' THEN 'site_protection_verify'
				WHEN parsed_text ILIKE '%cookies are disabled%requires cookies for authentication%' THEN 'wiley_cookies_disabled'
				WHEN parsed_text ILIKE '%enable javascript%required%' OR parsed_text ILIKE '%enable it to continue%' THEN 'js_required'
				WHEN parsed_text ILIKE '%connection timed out%error code%' THEN 'cdn_5xx_landing'
				WHEN parsed_text ILIKE '%the page isn''t redirecting properly%' THEN 'redirect_broken'
				WHEN parsed_text ILIKE '%page not found%' AND length(parsed_text) < 500 THEN 'page_not_found_short'
				WHEN parsed_text ILIKE '%access denied%' THEN 'access_denied_landing'
				WHEN parsed_text ILIKE '%please verify you are a human%' OR parsed_text ILIKE '%are you a robot%' THEN 'human_check'
				WHEN parsed_text ILIKE '<iframe title="Google Tag Manager"%' THEN 'perimeterx_iframe_leak'
				ELSE 'other_short'
			END AS silent_mode,
			COUNT(*) AS n,
			MIN(length(parsed_text)) AS min_len,
			MAX(length(parsed_text)) AS max_len
		FROM okt_repository.sources
		WHERE status IN ('fetched','processed')
			AND length(parsed_text) < 500
		GROUP BY 1
		ORDER BY n DESC;`)
	if err != nil {
		log.Printf("silent failures: %v", err)
		return
	}
	defer rows.Close()
	fmt.Printf("%-28s %6s %8s %8s\n", "silent_mode", "n", "min_len", "max_len")
	for rows.Next() {
		var mode string
		var n, minLen, maxLen int
		if err := rows.Scan(&mode, &n, &minLen, &maxLen); err != nil {
			log.Printf("silent failures scan: %v", err)
			continue
		}
		fmt.Printf("%-28s %6d %8d %8d\n", mode, n, minLen, maxLen)
	}
	fmt.Println()
}

func printTopFailingHosts(ctx context.Context, pool *pgxpool.Pool) {
	fmt.Println("=== Top failing hosts (>=3 attempts, ordered by failed count) ===")
	rows, err := pool.Query(ctx, `
		SELECT host, fetched_n, failed_n, fetched_n + failed_n AS total,
		       ROUND(100.0 * failed_n / NULLIF(fetched_n + failed_n, 0), 1) AS fail_pct
		FROM (
			SELECT substring(url from 'https?://([^/]+)') AS host,
			       COUNT(*) FILTER (WHERE status IN ('fetched','processed')) AS fetched_n,
			       COUNT(*) FILTER (WHERE status='failed') AS failed_n
			FROM okt_repository.sources
			GROUP BY 1
		) t
		WHERE fetched_n + failed_n >= 3
		ORDER BY failed_n DESC NULLS LAST
		LIMIT 25;`)
	if err != nil {
		log.Printf("top failing hosts: %v", err)
		return
	}
	defer rows.Close()
	fmt.Printf("%-36s %8s %8s %6s %8s\n", "host", "fetched", "failed", "total", "fail_pct")
	for rows.Next() {
		var host string
		var fetched, failed, total int
		var failPct float64
		if err := rows.Scan(&host, &fetched, &failed, &total, &failPct); err != nil {
			log.Printf("top failing hosts scan: %v", err)
			continue
		}
		if host == "" {
			host = "(unknown)"
		}
		fmt.Printf("%-36s %8d %8d %6d %7.1f%%\n", host, fetched, failed, total, failPct)
	}
	fmt.Println()
}