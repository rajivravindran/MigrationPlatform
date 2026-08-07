import type { Config } from "tailwindcss";

const config: Config = {
  content: ["./app/**/*.{ts,tsx}", "./components/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        brand: {
          50: "#eef5ff",
          500: "#3763d9",
          600: "#2a4fbf",
          700: "#22409f"
        }
      }
    }
  },
  plugins: []
};
export default config;
