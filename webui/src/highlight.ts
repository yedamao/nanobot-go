// rehypeHighlight highlights fenced code that names a language, using
// lowlight (highlight.js to hast, no HTML strings) with a curated set of
// grammars so the bundle stays small. Unknown languages stay plain.
import bash from "highlight.js/lib/languages/bash";
import c from "highlight.js/lib/languages/c";
import cpp from "highlight.js/lib/languages/cpp";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import dockerfile from "highlight.js/lib/languages/dockerfile";
import go from "highlight.js/lib/languages/go";
import ini from "highlight.js/lib/languages/ini";
import java from "highlight.js/lib/languages/java";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import makefile from "highlight.js/lib/languages/makefile";
import markdown from "highlight.js/lib/languages/markdown";
import python from "highlight.js/lib/languages/python";
import rust from "highlight.js/lib/languages/rust";
import shell from "highlight.js/lib/languages/shell";
import sql from "highlight.js/lib/languages/sql";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";
import type { Element, ElementContent, Root, RootContent } from "hast";
import { createLowlight } from "lowlight";

const lowlight = createLowlight({
  bash, c, cpp, css, diff, dockerfile, go, ini, java, javascript, json,
  makefile, markdown, python, rust, shell, sql, typescript, xml, yaml,
});
lowlight.registerAlias({ ini: ["toml"], shell: ["console"], bash: ["zsh"] });

function language(code: Element): string | undefined {
  const cls = code.properties.className;
  if (!Array.isArray(cls)) return undefined;
  for (const v of cls) {
    const s = String(v);
    if (s.startsWith("language-")) return s.slice(9).toLowerCase();
  }
  return undefined;
}

const text = (n: ElementContent): string =>
  n.type === "text" ? n.value : n.type === "element" ? n.children.map(text).join("") : "";

function walk(node: Root | Element) {
  for (const child of node.children as RootContent[]) {
    if (child.type !== "element") continue;
    const code = child.children[0];
    if (child.tagName === "pre" && code?.type === "element" && code.tagName === "code") {
      const lang = language(code);
      if (lang && lowlight.registered(lang)) {
        const result = lowlight.highlight(lang, code.children.map(text).join(""));
        code.properties.className = ["hljs", ...(code.properties.className as string[])];
        code.children = result.children as ElementContent[];
      }
      continue;
    }
    walk(child);
  }
}

export default function rehypeHighlight() {
  return (tree: Root) => walk(tree);
}
