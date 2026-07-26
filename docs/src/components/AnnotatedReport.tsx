// @ts-check
import React, { useEffect, useRef, useState, useCallback } from "react";

/**
 * AnnotatedReport — renders a static meta-synthesis snapshot (parsed from a
 * synthesis markdown file via scripts/parse_examples/parse.mjs) as an
 * interactive document where every citation is a clickable superscript that
 * opens a popover showing the supporting fact text and its source URLs.
 *
 * No API calls. Everything is self-contained in the `snapshot` prop.
 *
 * Two citation kinds are distinguished in the UI:
 *
 *   - **auto** (auto-matched facts) — `[N]` markers. These are facts the
 *     system retrieved automatically from the knowledge base by embedding
 *     similarity, after the synthesis was written, to give the reader an
 *     additional audit trail against the broad corpus. Rendered in the
 *     primary color.
 *   - **direct** (direct cites) — `[D{N}]` markers. These are facts the
 *     researcher / AI researcher explicitly selected from a source while
 *     writing the synthesis. They carry the researcher's intent and are the
 *     primary evidentiary backbone. Rendered with an amber accent and a
 *     leading "D" so they stand out from auto-matched annotations at a
 *     glance. Their popovers carry a "Direct cite" / "Auto-matched" label
 *     in the header so the reader always knows which kind they are reading.
 *
 * Snapshot shape (see docs/docs/reference/examples/_parsed/*.json):
 *   {
 *     title: string,
 *     bodyHtml: string,            // pre-rendered HTML with <sup class="okt-cite" ...> tags
 *     facts: {                     // auto-matched facts (keyed by N)
 *       "1": { text, sources, posture? },
 *       ...
 *     },
 *     directCites: {               // direct cites (keyed by N, the digit after "D")
 *       "1": { text, sources, posture? } | { unavailable: true },
 *       ...
 *     },
 *     citationsUsed: number[],     // auto-matched [N] markers used in body
 *     directCitesUsed: number[],   // [D{N}] direct-cite markers used in body
 *   }
 *
 * Backward compatibility: older snapshots (produced before direct cites
 * existed) have no `directCites` field and their sup tags carry no
 * `data-kind` attribute. Such snapshots render exactly as before — every
 * citation is treated as `auto`.
 */

/**
 * @typedef {{ text: string; sources: string[]; posture?: "supports" | "contradicts" | "related" }} FactEntry
 */
/**
 * @typedef {{
 *   title: string;
 *   bodyHtml: string;
 *   facts: Record<string, FactEntry>;
 *   directCites?: Record<string, FactEntry | { unavailable: true }>;
 *   citationsUsed: number[];
 *   directCitesUsed?: number[];
 * }} Snapshot
 */

/**
 * @param {{ snapshot: Snapshot; generatorModel?: string; banner?: React.ReactNode }} props
 */
export default function AnnotatedReport({ snapshot, generatorModel, banner }) {
  const containerRef = useRef(/** @type {HTMLDivElement|null} */ (null));
  /** @type {[any, any]} */
  const [popover, setPopover] = useState(null); // { n, kind, label, anchorRect, fact, unavailable }
  const popoverRef = useRef(/** @type {HTMLDivElement|null} */ (null));

  // Look up a fact by (kind, n). `kind` is "direct" or "auto". Returns
  // either the fact entry, or `{ unavailable: true }` if the annex listed
  // the marker as missing, or `null` if no entry exists at all.
  const lookupFact = useCallback(
    (/** @type {string} */ kind, /** @type {number} */ n) => {
      const key = String(n);
      if (kind === "direct") {
        return snapshot.directCites?.[key] ?? null;
      }
      return snapshot.facts[key] ?? null;
    },
    [snapshot]
  );

  const openCitation = useCallback(
    (/** @type {number} */ n, /** @type {string} */ kind, /** @type {DOMRect} */ anchorRect) => {
      const fact = lookupFact(kind, n);
      // Even when the fact is missing or unavailable, we still open the
      // popover so the reader gets feedback ("unavailable" / "no entry")
      // instead of a silent no-op.
      const label = kind === "direct" ? `D${n}` : `${n}`;
      setPopover({ n, kind, label, anchorRect, fact });
    },
    [lookupFact]
  );

  const closePopover = useCallback(() => setPopover(null), []);

  // Event delegation: any click on a .okt-cite inside the container opens it.
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const onClick = (/** @type {MouseEvent} */ e) => {
      const target = /** @type {HTMLElement} */ (e.target);
      const sup = target.closest(".okt-cite");
      if (!sup) return;
      e.preventDefault();
      e.stopPropagation();
      const n = parseInt(sup.getAttribute("data-n") || "0", 10);
      if (!n) return;
      // Older snapshots have no data-kind attribute — treat them as "auto".
      const kind = sup.getAttribute("data-kind") === "direct" ? "direct" : "auto";
      openCitation(n, kind, sup.getBoundingClientRect());
    };
    container.addEventListener("click", onClick);
    return () => container.removeEventListener("click", onClick);
  }, [openCitation]);

  // Close on Escape, close on click outside the popover, reposition on scroll/resize.
  useEffect(() => {
    if (!popover) return;
    const onKey = (/** @type {KeyboardEvent} */ e) => {
      if (e.key === "Escape") closePopover();
    };
    const onScroll = () => {
      // Re-anchor the popover to its (possibly moved) sup element.
      const sup = containerRef.current?.querySelector(
        `.okt-cite[data-n="${popover.n}"][data-kind="${popover.kind}"]`
      );
      if (sup) {
        setPopover((p) => (p ? { ...p, anchorRect: sup.getBoundingClientRect() } : p));
      } else {
        closePopover();
      }
    };
    const onOutside = (/** @type {MouseEvent} */ e) => {
      const target = /** @type {Node} */ (e.target);
      if (popoverRef.current && popoverRef.current.contains(target)) return;
      // Don't close if clicking another citation (it will re-open).
      const sup = target instanceof Element ? target.closest?.(".okt-cite") : null;
      if (sup) return;
      closePopover();
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("click", onOutside, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onScroll);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("click", onOutside, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onScroll);
    };
  }, [popover, closePopover]);

  // Compute popover position relative to viewport, flipping/adjusting if it
  // would overflow the window.
  let popoverStyle = /** @type {React.CSSProperties} */ ({ display: "none" });
  if (popover) {
    const pad = 8;
    const w = Math.min(480, window.innerWidth - 32);
    const left = Math.max(
      pad,
      Math.min(popover.anchorRect.left, window.innerWidth - w - pad)
    );
    // Place below the citation; if not enough room, place above.
    const below = popover.anchorRect.bottom + 8;
    const above = popover.anchorRect.top - 8;
    const spaceBelow = window.innerHeight - below;
    const placeBelow = spaceBelow > Math.min(280, window.innerHeight / 2) || above < 8;
    popoverStyle = {
      position: "fixed",
      left: `${left}px`,
      width: `${w}px`,
      maxHeight: "60vh",
      ...(placeBelow ? { top: `${below}px` } : { bottom: `${window.innerHeight - above}px` }),
      zIndex: 1000,
    };
  }

  return (
    <div className="okt-annotated-report" ref={containerRef}>
      {banner && <div className="okt-report-banner">{banner}</div>}
      {generatorModel && (
        <div className="okt-report-generator" role="note">
          <span className="okt-report-generator__label">Synthesis model:</span>{" "}
          <span className="okt-report-generator__model">{generatorModel}</span>
        </div>
      )}
      <div
        className="okt-report-body markdown"
        // bodyHtml is produced by our own build-time parser from a trusted
        // local markdown file; it never contains user input. Safe to inject.
        dangerouslySetInnerHTML={{ __html: snapshot.bodyHtml }}
      />

      {popover && (
        <div
          className={`okt-cite-popover card okt-cite-popover--${popover.kind}`}
          ref={popoverRef}
          style={popoverStyle}
          role="dialog"
          aria-label={`Citation [${popover.label}]`}
        >
          <div className="okt-cite-popover__header">
            <span className="okt-cite-popover__num">[{popover.label}]</span>
            <span
              className={`okt-cite-kind okt-cite-kind--${popover.kind}`}
              title={
                popover.kind === "direct"
                  ? "Direct cite — a fact the researcher explicitly chose from a source while writing the synthesis."
                  : "Auto-matched — a fact the system retrieved automatically from the knowledge base by embedding similarity."
              }
            >
              {popover.kind === "direct" ? "direct cite" : "auto-matched"}
            </span>
            {popover.fact && !popover.fact.unavailable && popover.fact.posture && (
              <span
                className={`okt-cite-posture okt-cite-posture--${popover.fact.posture}`}
                title={`This fact ${popover.fact.posture} the sentence it cites.`}
              >
                {popover.fact.posture}
              </span>
            )}
            <button
              className="okt-cite-popover__close clean-btn button"
              onClick={closePopover}
              aria-label="Close citation"
            >
              ✕
            </button>
          </div>
          {popover.fact && !popover.fact.unavailable ? (
            <>
              <div className="okt-cite-popover__text">{popover.fact.text}</div>
              {popover.fact.sources && popover.fact.sources.length > 0 && (
                <div className="okt-cite-popover__sources">
                  <span className="okt-cite-popover__sources-label">
                    Source{popover.fact.sources.length > 1 ? "s" : ""}:
                  </span>
                  <ul>
                    {popover.fact.sources.map((url, idx) => (
                      <li key={idx}>
                        <a href={url} target="_blank" rel="noopener noreferrer">
                          {prettyUrl(url)}
                        </a>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </>
          ) : (
            <div className="okt-cite-popover__text okt-cite-popover__text--unavailable">
              {popover.fact && popover.fact.unavailable
                ? "This fact was marked unavailable in the source annex (the underlying record could not be retrieved when the synthesis was exported)."
                : "No fact entry was found for this citation in the snapshot."}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** Make a URL more readable for display. */
function prettyUrl(/** @type {string} */ url) {
  let u = url;
  try {
    const parsed = new URL(url);
    u = parsed.hostname + (parsed.pathname && parsed.pathname !== "/" ? parsed.pathname : "");
    if (parsed.search) u += parsed.search.slice(0, 24) + (parsed.search.length > 24 ? "…" : "");
  } catch {
    // not a valid URL; return as-is
  }
  return u.length > 70 ? u.slice(0, 67) + "…" : u;
}