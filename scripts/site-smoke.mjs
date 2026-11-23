import { readFile, access } from "node:fs/promises";
import { resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const html = await readFile(resolve(root, "docs/index.html"), "utf8");
const readme = await readFile(resolve(root, "README.md"), "utf8");
const design = await readFile(resolve(root, "docs/technical-design.md"), "utf8");
const roadmap = await readFile(resolve(root, "docs/implementation-roadmap.md"), "utf8");

const requiredIds = [
  "overview", "architecture", "tenancy", "identity", "upload", "archive",
  "publish", "data", "api", "gateway", "runtime", "deployment",
  "threats", "testing", "roadmap", "opencode", "decisions", "references"
];

const failures = [];
for (const id of requiredIds) {
  if (!html.includes(`id="${id}"`)) failures.push(`missing section: ${id}`);
  if (!html.includes(`href="#${id}"`)) failures.push(`missing navigation link: ${id}`);
}
for (const token of [
  "Workspace RBAC", "Artifact ACL", "Quarantine", "current_version_id",
  "Ticket Exchange", "ZIP Slip", "Static Gateway", "OpenCode Web"
]) {
  if (!html.includes(token)) failures.push(`missing design topic: ${token}`);
}
if (!html.includes("visual-platform-core-technical-design.pdf")) {
  failures.push("missing PDF download link");
}
if (!readme.includes("docs/index.html")) failures.push("README does not link to HTML");
if (!readme.includes("docs/visual-platform-core-technical-design.pdf")) failures.push("README does not link to PDF");
if (!design.includes("不可变版本") || !design.includes("默认拒绝")) {
  failures.push("technical design source is missing core invariants");
}
if (!roadmap.includes("P0 上线阻断项") || !roadmap.includes("M9 OpenCode Web")) {
  failures.push("implementation roadmap is incomplete");
}
if (/<script[^>]+src=["']https?:/i.test(html)) {
  failures.push("external script dependency is not allowed in the documentation page");
}

try {
  await access(resolve(root, "docs/adr/0001-content-origin-isolation.md"));
  await access(resolve(root, "docs/adr/0002-immutable-publication.md"));
  await access(resolve(root, "docs/adr/0003-postgres-job-queue.md"));
  await access(resolve(root, "docs/adr/0004-static-only-mvp.md"));
} catch {
  failures.push("one or more ADR documents are missing");
}

if (failures.length) {
  console.error("Documentation smoke checks failed:");
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log(`Documentation smoke checks passed: ${requiredIds.length} sections, design topics, roadmap, ADRs, and PDF links.`);
