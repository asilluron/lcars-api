import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The page is served by lcars-api at /admin/, from files embedded in the
// binary (internal/adminui), so it is built there with relative URLs.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "../internal/adminui/dist",
    // The Makefile empties it, keeping the .gitkeep that lets Go build
    // without it.
    emptyOutDir: false,
  },
  server: {
    // `npm run dev` serves the page at http://localhost:5173/, and sends its
    // API calls (api/...) to an lcars-api running locally on port 8080.
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", rewrite: (path) => "/admin" + path },
    },
  },
});
