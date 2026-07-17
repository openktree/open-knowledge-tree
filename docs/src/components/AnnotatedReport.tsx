// @ts-check
import React, { useEffect, useRef, useState, useCallback } from "react";

/**
 * AnnotatedReport — renders a static meta-synthesis snapshot (parsed from a
 * synthesis markdown file via scripts/parse_examples/parse.mjs) as an
 * interactive document where every [N] citation is a clickable superscript
 * that opens a popover showing the supporting fact text and its source URLs.
 *
 * No API calls. Everything is self-contained in the `snapshot` prop.
 *
 * Snapshot shape (see docs/docs/reference/examples/_parsed/*.json):
 *   {
 *     title: string,
 *     bodyHtml: string,            // pre-rendered HTML with <sup class="okt-cite" data-n="N"> tags
 *     facts: { "1": { text: string, sources: string[] }, ... },
 *     citationsUsed: number[],
 *   }
 */

/**
 * @typedef {{ title: string; bodyHtml: string; facts: Record<string, { text: string; sources: string[] }>; citationsUsed: number[]; }} Snapshot
 */

/**
 * @param {{ snapshot: Snapshot }} props
 */
export default function AnnotatedReport({ snapshot }) {
  const containerRef = useRef(/** @type {HTMLDivElement|null} */ (null));
  /** @type {[any, any]} */
  const [popover, setPopover] = useState(null); // { n, anchorRect, fact }
  const popoverRef = useRef(/** @type {HTMLDivElement|null} */ (null));

  const openCitation = useCallback(
    (/** @type {number} */ n, /** @type {DOMRect} */ anchorRect) => {
      const fact = snapshot.facts[String(n)];
      if (!fact) return;
      setPopover({ n, anchorRect, fact });
    },
    [snapshot]
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
      openCitation(n, sup.getBoundingClientRect());
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
        `.okt-cite[data-n="${popover.n}"]`
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
      <div
        className="okt-report-body markdown"
        // bodyHtml is produced by our own build-time parser from a trusted
        // local markdown file; it never contains user input. Safe to inject.
        dangerouslySetInnerHTML={{ __html: snapshot.bodyHtml }}
      />

      {popover && (
        <div
          className="okt-cite-popover card"
          ref={popoverRef}
          style={popoverStyle}
          role="dialog"
          aria-label={`Citation [${popover.n}]`}
        >
          <div className="okt-cite-popover__header">
            <span className="okt-cite-popover__num">[{popover.n}]</span>
            <button
              className="okt-cite-popover__close clean-btn button"
              onClick={closePopover}
              aria-label="Close citation"
            >
              ✕
            </button>
          </div>
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