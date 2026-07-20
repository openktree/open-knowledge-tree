import typography from "@tailwindcss/typography";

/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        primary: {
          DEFAULT: "rgb(var(--okt-primary) / <alpha-value>)",
          hover: "rgb(var(--okt-primary-hover) / <alpha-value>)",
          soft: "rgb(var(--okt-primary-soft) / <alpha-value>)",
          fg: "rgb(var(--okt-primary-fg) / <alpha-value>)",
          ring: "rgb(var(--okt-primary-ring) / <alpha-value>)",
        },
        success: "rgb(var(--okt-success) / <alpha-value>)",
        info: "rgb(var(--okt-info) / <alpha-value>)",
        warning: "rgb(var(--okt-warning) / <alpha-value>)",
        danger: "rgb(var(--okt-danger) / <alpha-value>)",
        link: "rgb(var(--okt-link) / <alpha-value>)",
        surface: "rgb(var(--okt-surface) / <alpha-value>)",
        page: "rgb(var(--okt-page) / <alpha-value>)",
        border: "rgb(var(--okt-border) / <alpha-value>)",
        "text-base": "rgb(var(--okt-text) / <alpha-value>)",
        "text-muted": "rgb(var(--okt-muted) / <alpha-value>)",
      },
      boxShadow: {
        card: "0 1px 3px rgba(0,0,0,.08), 0 1px 2px rgba(0,0,0,.04)",
        "card-dark": "0 1px 3px rgba(0,0,0,.4)",
      },
      ringColor: {
        DEFAULT: "rgb(var(--okt-primary-ring) / <alpha-value>)",
      },
    },
  },
  plugins: [typography],
};
