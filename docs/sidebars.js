// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  reference: [
    "intro",
    {
      type: "category",
      label: "Reference",
      link: { type: "doc", id: "reference/overview" },
      items: [
        {
          type: "category",
          label: "Concepts",
          link: { type: "doc", id: "reference/concepts/overview" },
          items: [
            "reference/concepts/facts",
            "reference/concepts/concepts-and-contexts",
            "reference/concepts/summaries-and-synthesis",
            "reference/concepts/reports-and-autoannotation",
          ],
        },
        {
          type: "category",
          label: "Knowledge Flow",
          link: { type: "doc", id: "reference/knowledge-flow/overview" },
          items: [
            "reference/knowledge-flow/1-source-extraction",
            "reference/knowledge-flow/2-fact-decomposition",
            "reference/knowledge-flow/3-embedding",
            "reference/knowledge-flow/4-deduplication",
            "reference/knowledge-flow/5-concept-alias-extraction",
            "reference/knowledge-flow/6-summaries",
            "reference/knowledge-flow/7-synthesis",
            "reference/knowledge-flow/concept-graph",
          ],
        },
        {
          type: "category",
          label: "Agentic Flow",
          link: { type: "doc", id: "reference/agentic-flow/overview" },
          items: [
            "reference/agentic-flow/1-research",
            "reference/agentic-flow/2-query",
            "reference/agentic-flow/3-reports",
          ],
        },
        {
          type: "category",
          label: "Examples",
          link: { type: "doc", id: "reference/examples/overview" },
          items: [
            "reference/examples/healing",
            "reference/examples/agroforestry",
          ],
        },
      ],
    },
  ],
  mcp: [
    {
      type: "category",
      label: "MCP Tools",
      link: { type: "doc", id: "mcp/overview" },
      items: ["mcp/getting-started", "mcp/tools"],
    },
  ],
  api: [
    {
      type: "category",
      label: "REST API",
      link: { type: "doc", id: "api/overview" },
      items: [
        "api/auth",
        "api/repositories",
        "api/sources",
        "api/facts",
        "api/concepts",
        "api/investigations",
        "api/reports",
        "api/providers",
        "api/tasks",
      ],
    },
  ],
  localDev: [
    {
      type: "category",
      label: "Local Dev",
      link: { type: "doc", id: "local-dev/overview" },
      items: [
        "local-dev/docker-compose",
        "local-dev/services-overview",
        "local-dev/troubleshooting",
      ],
    },
  ],
  architecture: [
    {
      type: "category",
      label: "Architecture",
      link: { type: "doc", id: "architecture/overview" },
      items: [
        "architecture/multi-database",
        "architecture/rbac",
        "architecture/providers",
        "architecture/task-manager",
        "architecture/qdrant",
        "architecture/schema",
      ],
    },
  ],
};

export default sidebars;