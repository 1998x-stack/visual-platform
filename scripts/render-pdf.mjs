import { chromium } from "playwright";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { stat } from "node:fs/promises";

const root = resolve(import.meta.dirname, "..");
const htmlPath = resolve(root, "docs/index.html");
const pdfPath = resolve(root, "docs/visual-platform-core-technical-design.pdf");

const browser = await chromium.launch({
  headless: true,
  args: ["--no-sandbox", "--disable-dev-shm-usage"],
});

try {
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1000 },
    deviceScaleFactor: 1,
  });

  await page.goto(pathToFileURL(htmlPath).href, { waitUntil: "load" });
  await page.emulateMedia({ media: "print" });
  await page.pdf({
    path: pdfPath,
    format: "A4",
    printBackground: true,
    preferCSSPageSize: true,
    displayHeaderFooter: true,
    headerTemplate: "<div></div>",
    footerTemplate:
      '<div style="width:100%;padding:0 15mm;font:8px sans-serif;color:#71817d;display:flex;justify-content:space-between"><span>VISUAL PLATFORM · CORE TECHNICAL DESIGN</span><span><span class="pageNumber"></span> / <span class="totalPages"></span></span></div>',
    margin: {
      top: "13mm",
      right: "13mm",
      bottom: "17mm",
      left: "13mm",
    },
  });

  const info = await stat(pdfPath);
  if (info.size < 20_000) {
    throw new Error(`PDF output is unexpectedly small: ${info.size} bytes`);
  }
  console.log(`PDF generated: ${pdfPath} (${info.size} bytes)`);
} finally {
  await browser.close();
}
