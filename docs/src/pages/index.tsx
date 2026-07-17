import React from "react";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import Layout from "@theme/Layout";
import Link from "@docusaurus/Link";
import PipelineDiagram from "../components/PipelineDiagram";

const FEATURES = [
  {
    title: "Knowledge Flow",
    to: "/docs/reference/knowledge-flow/overview",
    desc: "The 7-stage pipeline: source extraction, fact decomposition with coreference resolution, embedding-based dedup, concept graph, summaries, and synthesis.",
  },
  {
    title: "MCP Tools",
    to: "/docs/mcp/overview",
    desc: "18 Model Context Protocol tools to fetch sources, search facts and concepts, track ingestion, and create annotated reports — all over OAuth 2.1.",
  },
  {
    title: "REST API",
    to: "/docs/api/overview",
    desc: "HTTP endpoints for auth, repositories, sources, facts, concepts, investigations, reports, and providers. JWT and OAuth 2.1 bearer auth.",
  },
  {
    title: "Local Dev",
    to: "/docs/local-dev/overview",
    desc: "Run the full stack with Docker Compose and `just dev`. Postgres, Qdrant, FlareSolverr, and MinIO boot in seconds.",
  },
  {
    title: "Architecture",
    to: "/docs/architecture/overview",
    desc: "Multi-database layout, Casbin RBAC, provider strategy, River task manager, Qdrant collections, and the full DB schema.",
  },
];

const EXAMPLES = [
  {
    title: "The Modular Tropical Agroforestry Recipe Book",
    to: "/docs/reference/examples/agroforestry",
    model: "GLM 5.2",
    desc: "A 4-scope meta-synthesis (~1,300 sources, 100,000+ facts) integrating tropical polyculture architecture, belowground mechanisms, mycorrhizal networks, and mushroom & pest ecology. Every citation is clickable and carries a posture.",
  },
  {
    title: "Human Alimentation: A Multidimensional Feeding Meta-Synthesis",
    to: "/docs/reference/examples/humanalimentation",
    model: "GPT 5.6 Sol",
    desc: "A 9-scope meta-synthesis (499 facts) integrating global foodways, historical diets, protein sources, nutrient matrices, lifespan evidence, mood & cognition, life-stage physiology, lived evidence, and governance. Every citation is clickable and carries a posture.",
  },
];

export default function Home(): React.ReactElement {
  const { siteConfig } = useDocusaurusContext();
  return (
    <Layout title={siteConfig.tagline} description={siteConfig.tagline}>
      <main style={{ maxWidth: 1100, margin: "0 auto", padding: "2rem 1rem" }}>
        <section style={{ textAlign: "center", padding: "3rem 0" }}>
          <h1 style={{ fontSize: "3rem", marginBottom: "0.5rem" }}>{siteConfig.title}</h1>
          <p style={{ fontSize: "1.25rem", color: "var(--ifm-color-emphasis-700)", maxWidth: 720, margin: "0 auto 1rem" }}>
            {siteConfig.tagline}
          </p>
          <p style={{ fontSize: "0.95rem", color: "var(--ifm-color-emphasis-600)", maxWidth: 640, margin: "0 auto 2rem" }}>
            Fetch a URL or DOI, and OKT extracts atomic facts, deduplicates them by meaning,
            links them into a concept graph, and accumulates syntheses you can search, cite, and reason over.
          </p>
          <div style={{ display: "flex", gap: "1rem", justifyContent: "center", flexWrap: "wrap" }}>
            <Link className="button button--primary button--lg" to="/docs/intro">
              Get Started
            </Link>
            <Link className="button button--secondary button--lg" to="/docs/reference/knowledge-flow/overview">
              Understand the System
            </Link>
          </div>
        </section>

        <section>
          <h2 style={{ textAlign: "center", marginBottom: "1.5rem" }}>The Data Pipeline at a Glance</h2>
          <PipelineDiagram />
        </section>

        <section style={{ padding: "2rem 0" }}>
          <h2 style={{ textAlign: "center", marginBottom: "0.5rem" }}>Live Examples</h2>
          <p style={{ textAlign: "center", color: "var(--ifm-color-emphasis-600)", maxWidth: 640, margin: "0 auto 1.5rem" }}>
            Real meta-syntheses produced by OKT&apos;s agentic flow. Click any citation to see the supporting fact and its sources.
            Each example header names the model that authored the synthesis.
          </p>
          <div className="grid" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(320px, 1fr))", gap: "1.5rem" }}>
            {EXAMPLES.map((ex) => (
              <Link key={ex.to} to={ex.to} className="card" style={{ padding: "1.5rem", textDecoration: "none", display: "block" }}>
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "0.75rem", marginBottom: "0.5rem" }}>
                  <h3 style={{ margin: 0 }}>{ex.title}</h3>
                </div>
                <span style={{ display: "inline-block", fontSize: "0.72rem", fontWeight: 700, padding: "0.1rem 0.5rem", borderRadius: 999, marginBottom: "0.6rem", background: "var(--ifm-color-primary-very-soft, rgba(79,70,229,0.12))", color: "var(--ifm-color-primary)" }}>
                  Synthesis model: {ex.model}
                </span>
                <p style={{ color: "var(--ifm-color-emphasis-600)", fontSize: "0.9rem", margin: 0 }}>{ex.desc}</p>
              </Link>
            ))}
          </div>
        </section>

        <section style={{ padding: "2rem 0" }}>
          <h2 style={{ textAlign: "center", marginBottom: "1.5rem" }}>Explore the Docs</h2>
          <div className="grid" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: "1.5rem" }}>
            {FEATURES.map((f) => (
              <Link key={f.title} to={f.to} className="card" style={{ padding: "1.5rem", textDecoration: "none", display: "block" }}>
                <h3 style={{ marginBottom: "0.5rem" }}>{f.title}</h3>
                <p style={{ color: "var(--ifm-color-emphasis-600)", fontSize: "0.9rem", margin: 0 }}>{f.desc}</p>
              </Link>
            ))}
          </div>
        </section>
      </main>
    </Layout>
  );
}