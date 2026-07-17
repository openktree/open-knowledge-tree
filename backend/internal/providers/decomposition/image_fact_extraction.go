package decomposition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// imageFactExtractionPrompt is the system prompt for the multimodal
// image fact extractor. It is deliberately fact-oriented rather than
// descriptive: the goal is to produce atomic, self-contained facts
// that the image conveys and that are relevant to the source topic,
// NOT generic image captions ("This image shows…"). The extracted
// facts flow through the same embedding + dedup pipeline as text
// facts, so they must be semantically comparable to text facts.
//
// The prompt mirrors the structure of the text fact-extraction
// prompt (self-containedness rules, what to skip) but is scoped to
// images. The model receives the source URL, source title, image alt
// text, and the image bytes as a multimodal user message; the
// returned strings become facts.text, and the worker sets
// facts.image_url separately from the image row's URL.
const imageFactExtractionPrompt = `You are a fact extraction and attribution system. Given an image from a knowledge source, extract ALL knowledge worth preserving.

## Source context
- Source URL: %s
- Source title: %s
- Image alt text (provided by the source page, may be empty): %s

## What to extract

Extract ONLY atomic, self-contained facts that the image conveys AND that are relevant to the source topic above. Each fact should be 1-4 sentences and verifiable from the image alone. Prefer fewer, more comprehensive facts over several short overlapping ones: when multiple candidate facts share the same subject or context, emit ONE descriptive fact that bundles them rather than one fact per datum.

Extract:
- **Specific data points from charts, graphs, tables, and diagrams** — with units, axis labels, time periods, and the named entity the data is about. "Revenue grew" is useless; "Acme Corp revenue grew 42% from 2020 to 2023 per the bar chart" is a fact.
- **Named entities depicted** — people, organisations, places, products, species, devices, artworks. Include the identifying label, caption, or legend text that names them.
- **Quantitative measurements shown** — dimensions, counts, percentages, rates, with units and the measured subject.
- **Procedure steps illustrated** — ordered steps in a workflow, method, assembly, or process. Preserve order; name each step.
- **Relationships or flows** — architecture diagrams, flowcharts, family trees, taxonomies. Name the nodes and the labelled edges.
- **Quoted text rendered in the image** — verbatim, with attribution if the image provides it. Code blocks, equations, and labelled formulas count here; name what the formula represents before the expression.
- **Definitions or classifications illustrated** — a taxonomy table, a named region on a map, a labelled cross-section. Name both the whole and the part.
- **Illustrative / descriptive facts** — when the image is an example, illustration, or photograph OF a named topical subject, capture that as a fact so the image can later be grouped under that concept. State what the image depicts AND what it is an example of, naming the subject explicitly. "This image illustrates the process of photosynthesis in a green plant" and "The image depicts the frond (leaf) of a palm tree, an example of pinnate compound leaf structure" are valid facts. "This image shows a plant" is not (no named subject). Include the source-topic framing when relevant: "The image is an anatomical illustration of the human heart used to explain inflammation of the myocardium."
- **Consolidation rule** — when several candidate facts concern the same subject, entity, chart, diagram, or process, merge them into a single comprehensive fact instead of emitting one fact per datum. Prefer a larger descriptive fact over multiple smaller overlapping facts, as long as every datum is verifiable from the image and the fact stays self-contained.
  - Bad: ["Acme Corp revenue in 2020 was $10M per the bar chart", "Acme Corp revenue in 2023 was $14.2M per the bar chart", "Acme Corp revenue grew 42% from 2020 to 2023 per the bar chart"]
  - Good: ["Acme Corp revenue grew 42% from $10M in 2020 to $14.2M in 2023 per the bar chart"]

## CRITICAL: Every fact must be self-contained

Each fact will be stored in a knowledge graph and read WITHOUT the original image or source. A reader seeing ONLY the fact text must understand it.

- Resolve all pronouns and demonstratives ("this", "that", "it", "they") to the explicit entity the image names. The source title / alt text tell you the topic; substitute the actual name.
- Name the subject. Never write "It reached 50%" — write "Acme Corp revenue reached 50% in 2023 according to the bar chart". Never write "The diagram shows the pipeline" — write "The diagram shows the data ingestion pipeline of the Acme Analytics platform". Never write "The image illustrates the process" — write "The image illustrates the process of photosynthesis in a green plant".
- Include the topic and context. If a fact describes a relationship, name both endpoints. "Latency dropped 40%" is useless — "The Acme Analytics recommendation pipeline p99 latency dropped 40% after the 2023 cache redesign" is a fact.
- Name specific events, places, methods, and entities the image provides. If the image does not name it, the fact cannot reference it.
- Fold the image's own attribution (chart source, figure caption, study citation) into the fact text so the fact carries its provenance.
- For illustrative / descriptive facts, name BOTH what the image depicts AND what concept it is an example of. "The image depicts the frond of a palm tree, an example of pinnate compound leaf structure" groups under both "palm tree" and "pinnate compound leaf". "The image illustrates photosynthesis" groups under "photosynthesis". A fact that names no concept ("a picture of a leaf") is worthless.

## What to SKIP — do NOT extract

- **Generic image descriptions** — "This image shows…", "A photo of…", "The image depicts…", "This is a chart of…". Never *only* describe the image; extract what the image SAYS. An illustrative fact that names the depicted subject ("The image illustrates the frond of a palm tree") is allowed; a bare caption ("A photo of a plant") is not.
- **Aesthetic, quality, or composition observations** — "a colourful diagram", "a high-resolution photo", "centered composition". These are not knowledge.
- **Layout, navigation, or UI chrome** — axis titles alone, legends alone, page numbers, watermarks, captions that name the figure number ("Figure 3") without content.
- **Brand logos, decorative elements, stock photos** — unless the logo or decoration itself carries topical signal (e.g. a labelled diagram where the brand IS the subject).
- **Decorative or non-topical images** — if the image is purely ornamental (a hero photo, a background pattern, a divider) and carries no signal about the source topic, return []. BUT if the image depicts, illustrates, or exemplifies a named topical subject (a plant part, an organ, a chemical structure, a piece of hardware, a step in a process), do NOT discard it — extract at least one illustrative fact naming the subject (see "Illustrative / descriptive facts" above).
- **Restating the alt text** — the alt text is a hint, not a fact. Only extract what the image actually conveys; do not echo the alt text back unless it is itself a verifiable fact about the topic.
- **Incomplete fragments** — bare noun phrases, prepositional phrases, labels without assertions.

**Rule of thumb**: if a reader seeing the fact without the image would ask "which one?", "what is this about?", or "where?" — the fact is not self-contained. Resolve it using the image's labels and the source context, or skip it. For an illustrative image, the reader must at least know which concept the image exemplifies — "this image illustrates photosynthesis" is acceptable; "this image illustrates a process" is not.

## Response format

Respond with a JSON array of strings, like: ["fact one", "fact two"]. Prefer fewer, richer facts over many narrow ones; do not emit a fact whose information is fully contained in another fact you are also emitting. If no facts can be extracted (the image is decorative or carries no topical signal), return []. Respond with ONLY the JSON array, no other text.`

// ImageFetcher is the contract the image fact extractor depends on
// for resolving inline image URLs to bytes. It is a subset of
// *fetch.FetchResolutionProvider (just FetchImageBytes) so the
// decomposition package does not import the fetch package directly
// (which would be a layering smell: decomposition is a consumer of
// AI + fetched content, not a peer of the fetch strategy). The
// fetch provider implements it; tests can inject a stub.
type ImageFetcher interface {
	FetchImageBytes(ctx context.Context, url string, maxBytes int64) ([]byte, string, error)
}

// ImageFactRequest is the per-image input to the image fact
// extractor. ImageBytes is non-empty for page renders (PDF) where
// the worker reads the bytes straight from source_images.bytes; for
// inline images the worker calls ImageFetcher to populate
// ImageBytes from ImageURL. ImageURL is always set and becomes
// facts.image_url on the resulting fact rows (the worker sets it,
// not the model, so the model never has to echo a URL).
//
// SourceHasText reports whether the source had parsed text that the
// text-chunk loop already processed (or will process). When true the
// extractor appends a "focus on figures" scope note to the prompt so
// the model does not waste effort re-transcribing text rendered in
// the image — the text pass already captured it. When false (e.g. an
// image-only / scanned PDF with no text layer, or an HTML source
// whose parser returned no body) the prompt stays generic and the
// model treats the image as the primary content. The worker computes
// this from source.ParsedText and passes it down so the provider
// stays transport-agnostic.
type ImageFactRequest struct {
	SourceURL     string
	SourceTitle   string
	ImageAlt      string
	ImageURL      string
	ImageBytes    []byte
	SourceHasText bool
	Attribution   FactExtractionAttribution
}

// ImageFactExtractionProvider is the multimodal analogue of
// FactExtractionProvider. It takes one image (plus source context)
// and returns zero or more fact strings. The worker calls it once
// per image attached to the source, after the text-chunk loop.
type ImageFactExtractionProvider interface {
	ExtractImageFacts(ctx context.Context, db store.DBTX, req ImageFactRequest) ([]string, error)
	Describe() ProviderDescription
}

// AIImageFactExtractionProvider is the AI-backed implementation. It
// builds a multimodal user message (text prompt with the source
// context + image bytes part) and calls the configured AI provider's
// Chat endpoint, then parses the response as a JSON array of
// strings using the same cleanJSONArray fallback as the text
// extractor. When the image bytes are empty (the fetch failed), it
// returns nil without calling the model — there is nothing to send.
type AIImageFactExtractionProvider struct {
	AIProvider    ai.AIProvider
	Model         string
	ImageFetcher  ImageFetcher
	MaxImageBytes int64
}

// NewAIImageFactExtractionProvider constructs an AI-backed image
// fact extractor. imageFetcher may be nil when the caller only ever
// passes requests with pre-populated ImageBytes (e.g. a test that
// seeds page render bytes); inline-image extraction will fail at
// fetch time if the fetcher is nil.
func NewAIImageFactExtractionProvider(
	aiProvider ai.AIProvider,
	model string,
	imageFetcher ImageFetcher,
	maxImageBytes int64,
) *AIImageFactExtractionProvider {
	if maxImageBytes <= 0 {
		maxImageBytes = 5 * 1024 * 1024 // 5 MB default
	}
	return &AIImageFactExtractionProvider{
		AIProvider:    aiProvider,
		Model:         model,
		ImageFetcher:  imageFetcher,
		MaxImageBytes: maxImageBytes,
	}
}

// Describe returns the static metadata for the AI-backed image fact
// extractor. Configured tracks whether both the underlying AI
// provider and the model name are set; a nil AIProvider or empty
// model means the worker logs "not configured" and skips image
// extraction (the source still produces text facts and is marked
// processed).
func (p *AIImageFactExtractionProvider) Describe() ProviderDescription {
	var aiDesc ai.ProviderDescription
	if p.AIProvider != nil {
		aiDesc = p.AIProvider.Describe()
	}
	configured := p.AIProvider != nil && aiDesc.Configured && p.Model != ""
	return ProviderDescription{
		Name:        "AI image fact extractor",
		Description: "Asks a configured multimodal chat model to enumerate the atomic, self-contained factual claims that an image (inline image or PDF page render) conveys about the source topic. Sends the source URL, title, image alt text, and the image bytes; requires a vision-capable model. Inline image URLs are fetched via the configured fetch provider before being sent.",
		Requires:    "providers.decomposition.image_extraction.{provider,model} and a multimodal/vision model, plus the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"image_extraction"},
		Notes:       "Provider is " + aiDesc.Name + ". The model MUST support image_url content parts (OpenAI vision format); a text-only model will error on every image. Per-image failures are logged and the image is skipped; the source is still marked processed.",
		Config: map[string]string{
			"ai_provider":      aiDesc.Name,
			"model":            p.Model,
			"max_image_bytes":  fmt.Sprintf("%d", p.MaxImageBytes),
		},
	}
}

// ExtractImageFacts sends the image plus source context to the
// multimodal model and returns the fact strings it produces. When
// ImageBytes is empty and ImageURL is set, the provider fetches the
// bytes first via the ImageFetcher (inline images). When the fetch
// fails or the bytes are empty, it returns nil without calling the
// model — there is nothing to send. The returned strings become
// facts.text; the worker sets facts.image_url separately.
func (p *AIImageFactExtractionProvider) ExtractImageFacts(ctx context.Context, db store.DBTX, req ImageFactRequest) ([]string, error) {
	bytes := req.ImageBytes
	if len(bytes) == 0 && req.ImageURL != "" && p.ImageFetcher != nil {
		fetched, ct, err := p.ImageFetcher.FetchImageBytes(ctx, req.ImageURL, p.MaxImageBytes)
		if err != nil {
			return nil, fmt.Errorf("image extraction: fetching image bytes: %w", err)
		}
		bytes = fetched
		req.ImageBytes = fetched
		_ = ct // content-type is sniffed again below from the bytes; kept for clarity
	}
	if len(bytes) == 0 {
		return nil, nil
	}

	contentType := sniffImageMIME(req.ImageBytes, req.ImageURL)
	if contentType == "" {
		return nil, nil
	}

	// Build the text prompt by substituting the three %s
	// placeholders in order: source URL, source title, image alt
	// text. We use strings.Replace (not fmt.Sprintf) because the
	// prompt body contains literal '%' characters (e.g. "42%",
	// "50%") that Sprintf would try to interpret as format verbs.
	prompt := buildImageFactExtractionPrompt(req.SourceURL, req.SourceTitle, req.ImageAlt, req.SourceHasText)

	var taskID *string
	if req.Attribution.TaskID != "" {
		taskID = &req.Attribution.TaskID
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "image_extraction",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.AIProvider.Chat(callCtx, db, ai.ChatRequest{
				Model: p.Model,
				Messages: []ai.ChatMessage{
					ai.NewImageMessage("user", prompt, []ai.ImageData{
						{Bytes: bytes, ContentType: contentType},
					}),
				},
				TaskID: taskID,
				Attribution: ai.Attribution{
					RepositoryID: req.Attribution.RepositoryID,
					SourceID:     req.Attribution.SourceID,
					Operation:    "image_extraction",
				},
			})
		},
	)
	if err != nil {
		return nil, fmt.Errorf("image extraction: ai chat failed: %w", err)
	}
	if len(resp.Messages) == 0 {
		return nil, nil
	}

	content := strings.TrimSpace(resp.Messages[0].Content)
	if content == "" || content == "[]" {
		return nil, nil
	}

	var facts []string
	if err := json.Unmarshal([]byte(content), &facts); err != nil {
		cleaned := cleanJSONArray(content)
		if cleaned != "" {
			if err2 := json.Unmarshal([]byte(cleaned), &facts); err2 != nil {
				return nil, fmt.Errorf("image extraction: failed to parse response as JSON array: %w (raw: %s)", err, content)
			}
		} else {
			return nil, fmt.Errorf("image extraction: failed to parse response as JSON array: %w (raw: %s)", err, content)
		}
	}
	return facts, nil
}

// imageFactExtractionFocusFiguresNote is appended to the base
// image fact-extraction prompt when the source the image belongs to
// already had its text body processed by the text-chunk loop. The
// note steers the vision model away from re-transcribing text that
// is merely rendered in the image (paragraphs, captions, body copy
// that the text pass already captured) and toward the visual
// information the text pass cannot see: charts, diagrams,
// photographs, labelled figures, etc. Without it the model tends to
// duplicate the text facts as "image" facts, polluting the knowledge
// graph with near-duplicates the dedup pass then has to clean up.
//
// It is NOT appended when the source had no parsed text (e.g. an
// image-only / scanned PDF with no text layer): in that case the
// image IS the primary content and the model should transcribe
// everything it can read, including rendered text.
const imageFactExtractionFocusFiguresNote = `

## Scope note

The text body of this source was already processed by a separate text-extraction pass, so the text facts already capture any prose, headings, captions, and body copy. DO NOT restate or transcribe text that is merely rendered in this image unless it conveys information the text pass could not capture. Focus on the visual information the text pass cannot see: figures, charts, graphs, diagrams, photographs, maps, labelled illustrations, and the data / relationships / named entities they depict. Verbatim text rendered inside a figure (axis labels, legend entries, a quoted formula, a code block) still counts — it is part of the figure, not the body — but pure body paragraphs visible in the image should be skipped.`

// buildImageFactExtractionPrompt substitutes the three %s
// placeholders (source URL, source title, image alt text) in the
// base prompt and, when sourceHasText is true, appends the
// focus-figures scope note. Extracted as a pure function so it can
// be unit-tested without constructing an AIImageFactExtractionProvider
// or stubbing the AI client.
func buildImageFactExtractionPrompt(sourceURL, sourceTitle, imageAlt string, sourceHasText bool) string {
	prompt := imageFactExtractionPrompt
	prompt = strings.Replace(prompt, "%s", defaultIfEmpty(sourceURL, "(unknown)"), 1)
	prompt = strings.Replace(prompt, "%s", defaultIfEmpty(sourceTitle, "(unknown)"), 1)
	prompt = strings.Replace(prompt, "%s", defaultIfEmpty(imageAlt, "(none)"), 1)
	if sourceHasText {
		prompt += imageFactExtractionFocusFiguresNote
	}
	return prompt
}

// sniffImageMIME resolves a usable image MIME type from the image
// bytes' magic number, falling back to the URL extension and finally
// to "image/png" so the multimodal data URL always carries a
// content-type. A nil/empty bytes slice returns "" so the caller
// can skip the call.
func sniffImageMIME(bytes []byte, url string) string {
	if len(bytes) == 0 {
		return ""
	}
	// Magic-number sniff for the common web image formats.
	switch {
	case len(bytes) >= 8 && string(bytes[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(bytes) >= 3 && string(bytes[:3]) == "\xff\xd8\xff":
		return "image/jpeg"
	case len(bytes) >= 12 && string(bytes[:4]) == "RIFF" && string(bytes[8:12]) == "WEBP":
		return "image/webp"
	case len(bytes) >= 6 && (string(bytes[:6]) == "GIF87a" || string(bytes[:6]) == "GIF89a"):
		return "image/gif"
	}
	// Fall back to URL extension.
	lower := strings.ToLower(url)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(lower, ".bmp"):
		return "image/bmp"
	}
	return "image/png"
}

func defaultIfEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}