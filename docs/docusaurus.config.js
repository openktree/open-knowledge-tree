import { themes } from "prism-react-renderer";

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: "Open Knowledge Tree",
  tagline: "The source-driven knowledge engine.",
  url: "https://open-knowledge-tree.dev",
  baseUrl: "/",
  favicon: "img/favicon.ico",
  organizationName: "open-knowledge-tree",
  projectName: "open-knowledge-tree-go",

  presets: [
    [
      "classic",
      /** @type {import('@docusaurus/preset-classic').Options} */
      {
        docs: {
          sidebarPath: "./sidebars.js",
          editUrl: "https://github.com/open-knowledge-tree/open-knowledge-tree-go/edit/main/docs/",
        },
        blog: false,
        theme: {
          customCss: "./src/css/custom.css",
        },
      },
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      colorMode: {
        defaultMode: "dark",
        respectPrefersColorScheme: true,
      },
      navbar: {
        title: "Open Knowledge Tree",
        logo: {
          alt: "OKT logo",
          src: "img/logo.svg",
        },
        items: [
          {
            type: "docSidebar",
            sidebarId: "reference",
            label: "Reference",
            position: "left",
          },
          {
            type: "docSidebar",
            sidebarId: "mcp",
            label: "MCP Tools",
            position: "left",
          },
          {
            type: "docSidebar",
            sidebarId: "api",
            label: "REST API",
            position: "left",
          },
          {
            type: "docSidebar",
            sidebarId: "architecture",
            label: "Architecture",
            position: "left",
          },
          {
            href: "https://github.com/open-knowledge-tree/open-knowledge-tree-go",
            label: "GitHub",
            position: "right",
          },
        ],
      },
      prism: {
        theme: themes.dracula,
        darkTheme: themes.dracula,
        additionalLanguages: ["bash", "json", "yaml", "sql", "go"],
      },
    }),
};

export default config;