import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": process.env.VITE_API_PROXY || "http://localhost:8080",
    },
  },
  // deck.gl and maplibre live in the lazily loaded WorldMap chunk, which exceeds the default
  // warning size on its own; everything else, including the landing route, stays small.
  build: { outDir: "dist", sourcemap: false, chunkSizeWarningLimit: 1700 },
});
