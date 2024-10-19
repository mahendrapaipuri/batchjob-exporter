import { themes as prismThemes } from "prism-react-renderer";
import type { Config } from "@docusaurus/types";
import type * as Preset from "@docusaurus/preset-classic";
import type * as Redocusaurus from "redocusaurus";

// Constants
const organizationName = "mahendrapaipuri";
const projectName = "ceems";

const config: Config = {
  title: "Compute Energy & Emissions Monitoring Stack (CEEMS)",
  tagline:
    "Monitor the energy consumption and carbon footprint of your workloads in realtime",
  favicon: "img/favicon.ico",

  // Set the production url of your site here
  url: `https://${organizationName}.github.io`,
  // Set the /<baseUrl>/ pathname under which your site is served
  // For GitHub pages deployment, it is often '/<projectName>/'
  baseUrl: `/${projectName}/`,

  // GitHub pages deployment config.
  // If you aren't using GitHub pages, you don't need these.
  organizationName: `${organizationName}`, // Usually your GitHub org/user name.
  projectName: `${projectName}`, // Usually your repo name.

  // NOTE: Set it to throw once docs are "release" ready
  onBrokenLinks: "warn",
  onBrokenMarkdownLinks: "warn",

  // Even if you don't use internationalization, you can use this field to set
  // useful metadata like html lang. For example, if your site is Chinese, you
  // may want to replace "en" with "zh-Hans".
  i18n: {
    defaultLocale: "en",
    locales: ["en"],
  },

  presets: [
    [
      "classic",
      {
        docs: {
          sidebarPath: "./sidebars.ts",
          // Please change this to your repo.
          // Remove this to remove the "edit this page" links.
          editUrl: `https://github.com/${organizationName}/${projectName}/tree/main/`,
        },
        blog: {
          showReadingTime: true,
          // Please change this to your repo.
          // Remove this to remove the "edit this page" links.
          editUrl: `https://github.com/${organizationName}/${projectName}/tree/main/`,
        },
        theme: {
          customCss: "./src/css/custom.css",
        },
      } satisfies Preset.Options,
    ],
    // Redocusaurus config
    [
      "redocusaurus",
      {
        // Plugin Options for loading OpenAPI files
        specs: [
          {
            // Redocusaurus will automatically bundle your spec into a single file during the build
            spec: "../pkg/api/http/docs/swagger.yaml",
            route: "/api/",
          },
        ],
        // Theme Options for modifying how redoc renders them
        theme: {
          // Change with your site colors
          primaryColor: "#3cc9beff",
        },
      },
    ] satisfies Redocusaurus.PresetEntry,
  ],

  themeConfig: {
    // Replace with your project's social card
    image: "img/docusaurus-social-card.jpg",
    docs: {
      sidebar: {
        // Make sidebar hideable
        hideable: true,
        // Collapse all sibling categories when expanding one category
        autoCollapseCategories: true,
      },
    },
    navbar: {
      title: "CEEMS",
      logo: {
        alt: "CEEMS Logo",
        src: "img/logo.svg",
      },
      items: [
        {
          type: "docSidebar",
          sidebarId: "ceemsSidebar",
          position: "left",
          label: "Documentation",
        },
        { to: "/api", label: "API", position: "left" },
        {
          href: `https://github.com/${organizationName}/${projectName}`,
          label: "GitHub",
          position: "right",
        },
      ],
    },
    footer: {
      style: "dark",
      links: [
        {
          title: "Docs",
          items: [
            {
              label: "CEEMS Docs",
              to: "/docs/",
            },
            {
              label: "Prometheus Docs",
              href: `https://prometheus.io/docs/introduction/overview/`,
            },
            {
              label: "Grafana Docs",
              href: `https://grafana.com/docs/`,
            },
          ],
        },
        {
          title: "Repository",
          items: [
            {
              label: "Issues",
              href: `https://github.com/${organizationName}/${projectName}/issues`,
            },
            {
              label: "Pull Requests",
              href: `https://github.com/${organizationName}/${projectName}/pulls`,
            },
            {
              label: "Discussions",
              href: `https://github.com/${organizationName}/${projectName}/discussions`,
            },
          ],
        },

        {
          title: "More",
          items: [
            {
              label: "GitHub",
              href: `https://github.com/${organizationName}/${projectName}`,
            },
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()}. Built with Docusaurus.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
    },
    algolia: {
      // The application ID provided by Algolia
      appId: 'KQ58EXUULN',
      // Public API key: it is safe to commit it
      apiKey: '16e0dc2efb99e9d654683fd9b9602082',
      indexName: 'mahendrapaipuriio',
      // // Optional: Replace parts of the item URLs from Algolia. Useful when using the same search index for multiple deployments using a different baseUrl. You can use regexp or string in the `from` param. For example: localhost:3000 vs myCompany.com/docs
      replaceSearchResultPathname: {
        from: '/ceems/',
        to: '/',
      },
      // We may need to tune `contextualSearch` and `searchParameters` to handle search for versioned docs
      // Optional: see doc section -- https://docusaurus.io/docs/search#contextual-search
      contextualSearch: false,
      // Optional: Algolia search parameters
      // searchParameters: {},
      // Optional: path for search page that enabled by default (`false` to disable it)
      searchPagePath: 'search',
    },

  } satisfies Preset.ThemeConfig,
};

export default config;
