import { readFile, writeFile } from "node:fs/promises";

import TurndownService from "turndown";
import turndownPluginGfm from "turndown-plugin-gfm";

const { tables } = turndownPluginGfm;

const htmlUrl = new URL("../docs.html", import.meta.url);
const markdownUrl = new URL("../docs.md", import.meta.url);
const checkOnly = process.argv.includes("--check");

const unexpectedArguments = process.argv.slice(2).filter((argument) => argument !== "--check");
if (unexpectedArguments.length > 0) {
  throw new Error(`unknown argument: ${unexpectedArguments[0]}`);
}

function capture(html, pattern, label) {
  const match = html.match(pattern);
  if (!match) {
    throw new Error(`could not find ${label} in docs.html`);
  }
  return match[1].trim();
}

function yamlString(value) {
  return JSON.stringify(value);
}

function codeLanguage(node, code) {
  const nodeClasses = node.getAttribute("class")?.split(/\s+/) ?? [];
  const parentClasses = node.parentNode?.getAttribute?.("class")?.split(/\s+/) ?? [];
  const trimmed = code.trimStart();

  if (nodeClasses.includes("docs-flow") || nodeClasses.includes("docs-synopsis")) {
    return "text";
  }
  if (parentClasses.includes("term")) {
    return "console";
  }
  const hasJsonBody = trimmed.startsWith("{") || trimmed.startsWith("[");
  const hasLeadingJsonComment = trimmed.startsWith("//") && /\n\s*[{[]/.test(trimmed);
  if (hasJsonBody || hasLeadingJsonComment) {
    return /^\s*\/\//m.test(code) ? "jsonc" : "json";
  }
  if (trimmed.startsWith("<")) {
    return "html";
  }
  return "bash";
}

function fencedCodeBlock(code, language) {
  const longestFence = Math.max(0, ...Array.from(code.matchAll(/`+/g), (match) => match[0].length));
  const fence = "`".repeat(Math.max(3, longestFence + 1));
  return `\n\n${fence}${language}\n${code}\n${fence}\n\n`;
}

function generateMarkdown(html) {
  const title = capture(html, /<title>([\s\S]*?)<\/title>/i, "the page title");
  const description = capture(
    html,
    /<meta\s+name="description"\s+content="([^"]*)">/i,
    "the meta description",
  );
  const canonicalUrl = capture(
    html,
    /<link\s+rel="canonical"\s+href="([^"]+)">/i,
    "the canonical URL",
  );
  let mainHtml = capture(
    html,
    /<main\b[^>]*class="[^"]*\bdocs-content\b[^"]*"[^>]*>([\s\S]*?)<\/main>/i,
    "the documentation content",
  );

  mainHtml = mainHtml.replace(
    /href=(["'])#([^"']+)\1/g,
    (_match, quote, fragment) => `href=${quote}${canonicalUrl}#${fragment}${quote}`,
  );

  const turndown = new TurndownService({
    bulletListMarker: "-",
    codeBlockStyle: "fenced",
    emDelimiter: "_",
    headingStyle: "atx",
    strongDelimiter: "**",
  });
  turndown.use(tables);
  turndown.remove((node) => {
    const classes = node.classList;
    return (
      (node.nodeName === "BUTTON" && classes?.contains("copy")) ||
      classes?.contains("term-bar")
    );
  });
  turndown.addRule("ardvarkCodeBlock", {
    filter: "pre",
    replacement(_content, node) {
      const code = node.textContent.replace(/\r\n?/g, "\n").replace(/^\n+|\n+$/g, "");
      return fencedCodeBlock(code, codeLanguage(node, code));
    },
  });

  const body = turndown
    .turndown(mainHtml)
    .replace(/^[ \t]+$/gm, "")
    .replace(/\n{3,}/g, "\n\n")
    .trim();

  const markdown = [
    "---",
    `title: ${yamlString(title)}`,
    `description: ${yamlString(description)}`,
    `canonical_url: ${yamlString(canonicalUrl)}`,
    "---",
    "",
    "<!-- Generated from docs.html by npm run docs:generate. Do not edit directly. -->",
    "",
    body,
    "",
  ].join("\n");

  const requiredContent = [
    "# ardvark documentation",
    "## Install",
    "## Programmatic use",
    "### MCP server",
    "| Tool | Arguments | What it does |",
    "## Commands",
    "## Operational notes",
  ];
  for (const expected of requiredContent) {
    if (!markdown.includes(expected)) {
      throw new Error(`generated docs.md is missing required content: ${expected}`);
    }
  }

  const forbiddenContent = ["<button", "<div", "data-copy=", "\ncopy\n"];
  for (const unexpected of forbiddenContent) {
    if (markdown.includes(unexpected)) {
      throw new Error(`generated docs.md contains UI-only content: ${unexpected}`);
    }
  }

  return markdown;
}

const html = await readFile(htmlUrl, "utf8");
const generated = generateMarkdown(html);

if (checkOnly) {
  let existing;
  try {
    existing = await readFile(markdownUrl, "utf8");
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
  }

  if (existing !== generated) {
    console.error("docs.md is stale; run `npm run docs:generate` in website/ and commit the result.");
    process.exitCode = 1;
  } else {
    console.log("docs.md matches docs.html");
  }
} else {
  await writeFile(markdownUrl, generated);
  console.log("generated docs.md from docs.html");
}
