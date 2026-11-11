// Draws the app icon and the two menu bar icons from the Atelier mark.
// Run with `npm run icons`, then `npx tauri icon src-tauri/icons/app-icon.png`.
import { Resvg } from "@resvg/resvg-js";
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const out = resolve(dirname(fileURLToPath(import.meta.url)), "../src-tauri/icons");
mkdirSync(out, { recursive: true });

const mark = (stroke, width) =>
  `<rect x="2" y="2" width="24" height="24" rx="7" stroke="${stroke}" stroke-width="${width}" fill="none"/>` +
  `<path d="M9 19 L14 8 L19 19 M11 15 H17" stroke="${stroke}" stroke-width="${width}" stroke-linecap="round" stroke-linejoin="round" fill="none"/>`;

const app = `<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">
  <rect x="100" y="100" width="824" height="824" rx="186" fill="#1a1917"/>
  <rect x="100" y="100" width="824" height="824" rx="186" fill="none" stroke="#3a3631" stroke-width="6"/>
  <g transform="translate(232 232) scale(20)">${mark("#f2b85c", 2)}</g>
</svg>`;

// Menu bar icons: black on clear, drawn as template images so macOS tints
// them. The second one carries a dot for "something needs you".
const tray = (dot) => `<svg xmlns="http://www.w3.org/2000/svg" width="44" height="44" viewBox="0 0 28 28">
  ${mark("#000000", 2.2)}
  ${dot ? '<circle cx="23.5" cy="4.5" r="4.5" fill="#000000" stroke="none"/>' : ""}
</svg>`;

const png = (svg, width) => new Resvg(svg, { fitTo: { mode: "width", value: width } }).render().asPng();
writeFileSync(resolve(out, "app-icon.png"), png(app, 1024));
writeFileSync(resolve(out, "tray.png"), png(tray(false), 44));
writeFileSync(resolve(out, "tray-dot.png"), png(tray(true), 44));
console.log("icons written to", out);
