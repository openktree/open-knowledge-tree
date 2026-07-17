import React from "react";
import Link from "@docusaurus/Link";

const STAGES = [
  { n: 1, title: "Source", desc: "Fetch URL/DOI via provider chain", href: "/docs/reference/knowledge-flow/1-source-extraction" },
  { n: 2, title: "Facts", desc: "Decompose into self-contained atomic facts", href: "/docs/reference/knowledge-flow/2-fact-decomposition" },
  { n: 3, title: "Embed", desc: "Vectorize facts into Qdrant", href: "/docs/reference/knowledge-flow/3-embedding" },
  { n: 4, title: "Dedup", desc: "Merge idea-level duplicates by cosine distance", href: "/docs/reference/knowledge-flow/4-deduplication" },
  { n: 5, title: "Concepts", desc: "Extract concepts, aliases, contexts -> graph", href: "/docs/reference/knowledge-flow/5-concept-alias-extraction" },
  { n: 6, title: "Summaries", desc: "Incremental per-concept synthesis slices", href: "/docs/reference/knowledge-flow/6-summaries" },
  { n: 7, title: "Synthesis", desc: "Authoritative definition per concept group", href: "/docs/reference/knowledge-flow/7-synthesis" },
];

const AGENTIC_PHASES = [
  { n: 1, title: "Research", desc: "search sources, fetch + process, open investigations", href: "/docs/reference/agentic-flow/1-research" },
  { n: 2, title: "Query", desc: "search facts + concepts, read syntheses, walk related", href: "/docs/reference/agentic-flow/2-query" },
  { n: 3, title: "Reports", desc: "author markdown, get back per-sentence fact annotations", href: "/docs/reference/agentic-flow/3-reports" },
];

export default function PipelineDiagram() {
  return (
    <div className="pipelineHero">
      <div className="pipelineRow">
        {STAGES.map((stage, i) => (
          <React.Fragment key={stage.n}>
            <Link to={stage.href} className="pipelineNode">
              <div className="pipelineBox">
                <div className="pipelineNum">Stage {stage.n}</div>
                <div className="pipelineTitle">{stage.title}</div>
                <div className="pipelineDesc">{stage.desc}</div>
              </div>
            </Link>
            {i < STAGES.length - 1 && (
              <div className="pipelineArrow" aria-hidden="true">
                &rarr;
              </div>
            )}
          </React.Fragment>
        ))}
      </div>

      <div className="pipelineRow agenticRow">
        {AGENTIC_PHASES.map((phase, i) => (
          <React.Fragment key={phase.n}>
            <Link to={phase.href} className="pipelineNode">
              <div className="pipelineBox agenticBox">
                <div className="pipelineNum agenticNum">Phase {phase.n}</div>
                <div className="pipelineTitle">{phase.title}</div>
                <div className="pipelineDesc">{phase.desc}</div>
              </div>
            </Link>
            {i < AGENTIC_PHASES.length - 1 && (
              <div className="pipelineArrow agenticArrow" aria-hidden="true">
                &harr;
              </div>
            )}
          </React.Fragment>
        ))}
      </div>
    </div>
  );
}