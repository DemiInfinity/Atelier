import { resolve } from "node:path";
import { defineConfig } from "vitest/config";

// Two pages: the main window and the menu bar popover.
export default defineConfig({
  clearScreen: false,
  server: { port: 1430, strictPort: true },
  build: {
    target: "es2022",
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
        tray: resolve(__dirname, "tray.html"),
      },
    },
  },
  test: { environment: "happy-dom", include: ["src/**/*.test.ts"] },
});
