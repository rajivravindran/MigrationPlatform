import type { Config } from "tailwindcss";

/**
 * Dark operations theme (product plan UX-02).
 *
 * The neutral scale is remapped so that the existing pages, written against
 * light-theme slate utilities (bg-white, text-slate-900, border-slate-200…),
 * render on the dark surface set without touching every file: "white" is the
 * raised surface, low slate steps are surfaces and borders, high steps are
 * text. Tinted status scales are inverted the same way (100 = tint, 800 = ink).
 */
const config: Config = {
  content: ["./app/**/*.{ts,tsx}", "./components/**/*.{ts,tsx}"],
  theme: {
    extend: {
      fontFamily: {
        serif: ["\"Iowan Old Style\"", "\"Palatino Linotype\"", "\"Book Antiqua\"", "Palatino", "Georgia", "\"Noto Serif\"", "serif"],
        sans: ["\"Iowan Old Style\"", "\"Palatino Linotype\"", "\"Book Antiqua\"", "Palatino", "Georgia", "\"Noto Serif\"", "serif"],
        mono: ["\"JetBrains Mono\"", "\"SFMono-Regular\"", "Menlo", "Consolas", "\"Liberation Mono\"", "monospace"]
      },
      colors: {
        white: "#151c2b",
        black: "#07090f",
        slate: {
          50: "#1a2234",
          100: "#1f293d",
          200: "#283449",
          300: "#344158",
          400: "#7f8ba3",
          500: "#9aa6bd",
          600: "#b7c1d3",
          700: "#cbd3e1",
          800: "#dfe5ef",
          900: "#eef2f8",
          950: "#f7f9fc"
        },
        brand: {
          50: "#18253f",
          100: "#1d2d4d",
          200: "#2a4070",
          300: "#3c5aa0",
          500: "#5b8def",
          600: "#6d9bf5",
          700: "#9dbcfb",
          800: "#c4d6fd"
        },
        emerald: { 50: "#11271c", 100: "#143322", 200: "#1d4a31", 300: "#23603f", 400: "#36b37e", 500: "#3fcf8e", 600: "#5edca3", 700: "#8ae7bd", 800: "#b4f0d4", 900: "#d4f7e6", 950: "#e9fbf2" },
        rose: { 50: "#2b1519", 100: "#3a1a1f", 200: "#532229", 300: "#6d2c35", 400: "#e05d5d", 500: "#f26d6d", 600: "#f58585", 700: "#f8a4a4", 800: "#fbc3c3", 900: "#fde0e0", 950: "#fff0f0" },
        amber: { 50: "#2a2111", 100: "#382b12", 200: "#4f3c17", 300: "#6a511f", 400: "#d9a441", 500: "#e8b04c", 600: "#edbe68", 700: "#f2cf8f", 800: "#f7e0b5", 900: "#fbeed6", 950: "#fdf6e8" },
        sky: { 50: "#10243a", 100: "#122c47", 200: "#173e63", 300: "#1e5283", 400: "#3fa7e8", 500: "#4fb3f0", 600: "#6fc2f4", 700: "#9bd5f8", 800: "#c3e6fb", 900: "#e0f2fd", 950: "#eef8fe" },
        indigo: { 50: "#1c1f3d", 100: "#23274d", 200: "#30366c", 300: "#40488e", 400: "#7b84f0", 500: "#8b93f4", 600: "#a3a9f6", 700: "#bfc3f9", 800: "#d7d9fb", 900: "#ebecfd", 950: "#f3f4fe" },
        violet: { 50: "#241a3d", 100: "#2d204d", 200: "#3f2d6c", 300: "#533d8e", 400: "#a37cf0", 500: "#b08cf4", 600: "#c0a3f6", 700: "#d3bff9", 800: "#e3d7fb", 900: "#f0eafd", 950: "#f7f3fe" },
        blue: { 50: "#15213b", 100: "#1a2a4b", 200: "#243b6b", 300: "#2f4e8f", 400: "#5b8def", 500: "#6d9bf5", 600: "#86adf7", 700: "#a8c4fa", 800: "#c9dafc", 900: "#e3ecfe", 950: "#eff4fe" }
      },
      boxShadow: {
        sm: "0 1px 0 0 rgb(0 0 0 / 0.25)",
        DEFAULT: "0 1px 2px 0 rgb(0 0 0 / 0.35)",
        md: "0 4px 12px -2px rgb(0 0 0 / 0.45)",
        lg: "0 12px 32px -8px rgb(0 0 0 / 0.55)"
      }
    }
  },
  plugins: []
};
export default config;
