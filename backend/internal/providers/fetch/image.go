package fetch

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// ErrImageTooLarge is returned by FetchImageBytes when the response
// body exceeds the caller-supplied maxBytes cap. Callers (the image
// fact extractor) treat this as a skip-this-image signal, not a hard
// failure, matching the per-chunk text error tolerance.
var ErrImageTooLarge = fmt.Errorf("image exceeds max bytes")

// FetchImageBytes fetches an image URL with the same browser-like
// headers the resolver uses for source pages and returns the raw
// bytes plus the sniffed content-type (e.g. "image/png"). It reuses
// the provider's *http.Client so the request shares timeouts /
// redirect behaviour with the source fetch path.
//
// The body is capped at maxBytes: if the response Content-Length
// advertises more, or the streaming read crosses the cap, the
// request is aborted with ErrImageTooLarge. This bounds memory use
// in the image fact extractor, which loads each image fully into
// memory to base64-encode it for the multimodal model.
//
// Content-type is sniffed from the response Content-Type header
// (lowercased, parameters stripped) with a URL-extension fallback
// for servers that send a generic "application/octet-stream" or no
// Content-Type at all.
func (p *FetchResolutionProvider) FetchImageBytes(ctx context.Context, url string, maxBytes int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating image request: %w", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "image")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("image fetch request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image fetch status %d", resp.StatusCode)
	}

	// Bound the read. A declared Content-Length above the cap is
	// rejected without reading; otherwise we cap the io.LimitReader
	// one byte above maxBytes so a body that exactly equals maxBytes
	// is accepted but a body one byte longer is detected as overflow.
	if resp.ContentLength > maxBytes {
		return nil, "", ErrImageTooLarge
	}
	limitReader := io.LimitReader(resp.Body, maxBytes+1)
	body, err := io.ReadAll(limitReader)
	if err != nil {
		return nil, "", fmt.Errorf("reading image body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, "", ErrImageTooLarge
	}

	contentType := sniffImageContentType(resp.Header.Get("Content-Type"), url)
	return body, contentType, nil
}

// sniffImageContentType resolves a usable image MIME type from a
// (possibly empty or generic) Content-Type header, falling back to
// the URL extension and finally to "image/png" as a last resort so
// the multimodal data URL always carries a content-type.
func sniffImageContentType(header, url string) string {
	mt, _, err := mime.ParseMediaType(header)
	if err == nil && mt != "" && mt != "application/octet-stream" {
		return strings.ToLower(mt)
	}
	// Fall back to extension.
	lower := strings.ToLower(url)
	for ext, ct := range map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".gif":  "image/gif",
		".svg":  "image/svg+xml",
		".bmp":  "image/bmp",
	} {
		if strings.HasSuffix(lower, ext) {
			return ct
		}
	}
	return "image/png"
}