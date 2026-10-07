// Markdown → safe HTML for answers: marked (GFM: tables, lists, fenced code), DOMPurify, and highlight.js with
// only the languages local models usually write.

import DOMPurify from "dompurify";
import hljs from "highlight.js/lib/core";
import bash from "highlight.js/lib/languages/bash";
import c from "highlight.js/lib/languages/c";
import cpp from "highlight.js/lib/languages/cpp";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import go from "highlight.js/lib/languages/go";
import java from "highlight.js/lib/languages/java";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import python from "highlight.js/lib/languages/python";
import rust from "highlight.js/lib/languages/rust";
import sql from "highlight.js/lib/languages/sql";
import swift from "highlight.js/lib/languages/swift";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";
import { Marked, type Tokens } from "marked";

const languages = { bash, c, cpp, css, diff, go, java, javascript, json, python, rust, sql, swift, typescript, xml, yaml };
for (const [name, lang] of Object.entries(languages)) hljs.registerLanguage(name, lang);
hljs.registerAliases(["sh", "shell", "zsh", "console"], { languageName: "bash" });
hljs.registerAliases(["js", "jsx", "mjs"], { languageName: "javascript" });
hljs.registerAliases(["ts", "tsx"], { languageName: "typescript" });
hljs.registerAliases(["html", "svg"], { languageName: "xml" });
hljs.registerAliases(["py"], { languageName: "python" });
hljs.registerAliases(["golang"], { languageName: "go" });
hljs.registerAliases(["yml"], { languageName: "yaml" });
hljs.registerAliases(["rs"], { languageName: "rust" });

const esc = (s: string) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

const copyIcon =
  '<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>';

const marked = new Marked({
  gfm: true,
  breaks: false,
  renderer: {
    code({ text, lang }: Tokens.Code) {
      const name = (lang ?? "").trim().split(/\s+/)[0].toLowerCase();
      const known = name && hljs.getLanguage(name);
      const body = known ? hljs.highlight(text, { language: name, ignoreIllegals: true }).value : esc(text);
      return (
        `<div class="md-code"><div class="md-code-bar"><span>${esc(name || "text")}</span>` +
        `<button type="button" data-copy title="Copy code">${copyIcon}<span>Copy</span></button></div>` +
        `<pre><code class="hljs">${body}</code></pre></div>`
      );
    },
    link({ href, title, tokens }: Tokens.Link) {
      const text = this.parser.parseInline(tokens);
      return `<a href="${esc(href)}" target="_blank" rel="noreferrer noopener"${title ? ` title="${esc(title)}"` : ""}>${text}</a>`;
    },
  },
});

DOMPurify.addHook("afterSanitizeAttributes", (node) => {
  if (node.tagName === "A") {
    node.setAttribute("target", "_blank");
    node.setAttribute("rel", "noreferrer noopener");
  }
});

/** Closes an unfinished code fence while the answer streams, so the rest does not jump between code and text. */
function closeFences(md: string) {
  const fences = md.match(/^\s*(```|~~~)/gm)?.length ?? 0;
  return fences % 2 ? md + "\n```" : md;
}

// thoughts: markdown, but HTML the model wrote is shown as text (it was thinking about it, not writing a page)
const thoughts = new Marked({
  gfm: true,
  breaks: true,
  renderer: {
    html({ text }: Tokens.HTML | Tokens.Tag) {
      return esc(text);
    },
    link({ href, title, tokens }: Tokens.Link) {
      const text = this.parser.parseInline(tokens);
      return `<a href="${esc(href)}" target="_blank" rel="noreferrer noopener"${title ? ` title="${esc(title)}"` : ""}>${text}</a>`;
    },
  },
});

/** Markdown → safe HTML. `thought`: the model's thinking (raw HTML escaped, single line breaks kept). */
export function renderMarkdown(md: string, streaming = false, thought = false): string {
  const html = (thought ? thoughts : marked).parse(streaming ? closeFences(md) : md, { async: false }) as string;
  return DOMPurify.sanitize(html, { ADD_ATTR: ["target", "data-copy"], FORBID_TAGS: ["style", "form", "img"] });
}
