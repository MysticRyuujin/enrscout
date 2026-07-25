import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": process.env.VITE_API_PROXY || "http://localhost:8080",
    },
  },
  // deck.gl is isolated in the lazy-loaded Overview route; the initial application
  // bundle stays small even though that optional visualization chunk is substantial.
  build: { outDir: "dist", sourcemap: false, chunkSizeWarningLimit: 1000 },
});
