// Markdown → DOM renderer for model output (spec §9.4). XSS-safe by
// construction: output is built exclusively with createElement/textContent,
// never innerHTML, so untrusted model text can never introduce markup.
// Supported: fenced code (+copy), # headings, **bold**, *italic*,
// ~~strikethrough~~, `code`, [links](http/https only), bare http(s) URLs
// (autolink), nested lists and task lists, > quotes, horizontal rules, and
// GitHub-style tables. Anything else is literal text.

const INLINE_RE =
  /`([^`\n]+)`|~~([^~\n]+)~~|\*\*([^*\n]+)\*\*|\*([^*\n]+)\*|\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)|(https?:\/\/[^\s<>"'`[\])]+)/g;

// Trailing sentence punctuation never belongs to a URL (balanced-paren
// endings like …) are already excluded by the character class).
function trimUrlTail(url) {
  let end = url.length;
  while (end > 0 && /[.,;:!?"'»…]/.test(url[end - 1])) end--;
  return url.slice(0, end);
}

function renderInline(text) {
  const frag = document.createDocumentFragment();
  let last = 0;
  let m;
  INLINE_RE.lastIndex = 0;
  while ((m = INLINE_RE.exec(text))) {
    if (m.index > last) frag.append(text.slice(last, m.index));
    if (m[1] !== undefined) {
      const code = document.createElement("code");
      code.textContent = m[1];
      frag.append(code);
    } else if (m[2] !== undefined) {
      const del = document.createElement("del");
      del.textContent = m[2];
      frag.append(del);
    } else if (m[3] !== undefined) {
      const b = document.createElement("strong");
      b.textContent = m[3];
      frag.append(b);
    } else if (m[4] !== undefined) {
      const em = document.createElement("em");
      em.textContent = m[4];
      frag.append(em);
    } else if (m[5] !== undefined) {
      const a = document.createElement("a");
      a.href = m[6]; // regex admits only http(s) URLs
      a.textContent = m[5];
      a.rel = "noopener noreferrer nofollow";
      a.target = "_blank";
      frag.append(a);
    } else {
      // Bare autolink: sentence punctuation stays text, the URL is the link.
      const url = trimUrlTail(m[7]);
      const a = document.createElement("a");
      a.href = url;
      a.textContent = url;
      a.rel = "noopener noreferrer nofollow";
      a.target = "_blank";
      frag.append(a, m[7].slice(url.length));
    }
    last = INLINE_RE.lastIndex;
  }
  frag.append(text.slice(last));
  return frag;
}

function codeBlock(code, lang) {
  const wrap = document.createElement("div");
  wrap.className = "codeblock";

  const bar = document.createElement("div");
  bar.className = "codebar";
  const label = document.createElement("span");
  label.className = "codelang";
  label.textContent = lang || "code";
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn";
  btn.textContent = "Copy";
  btn.addEventListener("click", async () => {
    let ok = true;
    try {
      await navigator.clipboard.writeText(code);
    } catch {
      ok = false;
    }
    btn.textContent = ok ? "Copied" : "Copy failed";
    setTimeout(() => {
      btn.textContent = "Copy";
    }, 1600);
  });
  bar.append(label, btn);

  const pre = document.createElement("pre");
  const codeEl = document.createElement("code");
  codeEl.textContent = code;
  pre.append(codeEl);

  wrap.append(bar, pre);
  return wrap;
}

// Split a table row without treating escaped pipes or pipes inside inline
// code as column boundaries. This is deliberately local to tables: ordinary
// prose keeps the renderer's existing literal-backslash behaviour.
function tableCells(line) {
  const cells = [];
  let cell = "";
  let inCode = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (ch === "\\" && line[i + 1] === "|") {
      cell += "|";
      i++;
    } else if (ch === "`") {
      inCode = !inCode;
      cell += ch;
    } else if (ch === "|" && !inCode) {
      cells.push(cell.trim());
      cell = "";
    } else {
      cell += ch;
    }
  }
  cells.push(cell.trim());

  // Leading and trailing pipes are table decoration, not empty columns.
  if (line.trimStart().startsWith("|") && cells[0] === "") cells.shift();
  if (line.trimEnd().endsWith("|") && cells.at(-1) === "") cells.pop();
  return cells;
}

function tableDefinition(lines, at) {
  if (at + 1 >= lines.length || !lines[at].includes("|")) return null;
  const headings = tableCells(lines[at]);
  const separators = tableCells(lines[at + 1]);
  if (
    headings.length === 0 ||
    headings.length !== separators.length ||
    !separators.every((cell) => /^:?-+:?$/.test(cell))
  ) return null;

  return {
    headings,
    alignments: separators.map((cell) =>
      cell.startsWith(":") && cell.endsWith(":") ? "center" :
      cell.endsWith(":") ? "right" : "left"),
  };
}

function tableElement(lines, at, definition) {
  const wrap = document.createElement("div");
  wrap.className = "md-table-wrap";
  const table = document.createElement("table");
  const head = document.createElement("thead");
  const headRow = document.createElement("tr");

  definition.headings.forEach((text, column) => {
    const th = document.createElement("th");
    th.scope = "col";
    th.className = `md-align-${definition.alignments[column]}`;
    th.append(renderInline(text));
    headRow.append(th);
  });
  head.append(headRow);
  table.append(head);

  const body = document.createElement("tbody");
  let i = at + 2;
  while (i < lines.length && lines[i].trim() !== "" && lines[i].includes("|")) {
    const values = tableCells(lines[i]);
    const row = document.createElement("tr");
    for (let column = 0; column < definition.headings.length; column++) {
      const td = document.createElement("td");
      td.className = `md-align-${definition.alignments[column]}`;
      td.append(renderInline(values[column] || ""));
      row.append(td);
    }
    body.append(row);
    i++;
  }
  table.append(body);
  wrap.append(table);
  return { element: wrap, next: i };
}

function indentation(raw) {
  let width = 0;
  for (const ch of raw) width += ch === "\t" ? 4 : 1;
  return width;
}

function listMarker(line) {
  const match = /^(\s*)([-+*]|\d+[.)])\s+(.*)$/.exec(line);
  if (!match) return null;
  const ordered = /^\d/.test(match[2]);
  return {
    indent: indentation(match[1]),
    ordered,
    number: ordered ? parseInt(match[2], 10) : 0,
    text: match[3],
  };
}

// Parse one list level. A more-indented marker recursively belongs to the
// preceding item; switching marker type at that indentation creates a sibling
// sublist. Explicit ordered-list values are retained rather than renumbered.
function listElement(lines, at, indent, ordered) {
  const list = document.createElement(ordered ? "ol" : "ul");
  let i = at;
  let lastItem = null;
  let firstNumber = null;

  while (i < lines.length) {
    const marker = listMarker(lines[i]);
    if (!marker || marker.indent < indent) break;

    if (marker.indent > indent) {
      if (!lastItem) break;
      const nested = listElement(lines, i, marker.indent, marker.ordered);
      lastItem.append(nested.element);
      i = nested.next;
      continue;
    }
    if (marker.ordered !== ordered) break;

    const li = document.createElement("li");
    if (ordered) {
      if (firstNumber === null) {
        firstNumber = marker.number;
        if (firstNumber !== 1) list.start = firstNumber;
      }
      li.value = marker.number;
    }

    const task = /^\[([ xX])\]\s+(.*)$/.exec(marker.text);
    if (task) {
      const checked = task[1].toLowerCase() === "x";
      const checkbox = document.createElement("input");
      checkbox.type = "checkbox";
      checkbox.checked = checked;
      checkbox.disabled = true;
      checkbox.setAttribute("aria-label", checked ? "Completed" : "Not completed");
      li.className = "task-list-item";
      list.classList.add("task-list");
      li.append(checkbox, renderInline(task[2]));
    } else {
      li.append(renderInline(marker.text));
    }
    list.append(li);
    lastItem = li;
    i++;

    // An indented, non-marker line continues the current item. Nested list
    // markers are left for the recursive branch above.
    while (i < lines.length && !listMarker(lines[i]) && lines[i].trim() !== "") {
      const continuation = /^(\s+)(.*)$/.exec(lines[i]);
      if (!continuation || indentation(continuation[1]) <= indent) break;
      li.append(document.createElement("br"), renderInline(continuation[2]));
      i++;
    }
  }

  return { element: list, next: i };
}

function horizontalRule(line) {
  return /^\s{0,3}(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$/.test(line);
}

/** Render full markdown text into a DocumentFragment. */
export function renderMarkdown(src) {
  const frag = document.createDocumentFragment();
  const lines = String(src).replace(/\r\n?/g, "\n").split("\n");
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code block (unclosed fences run to end of message).
    const fence = /^```(.*)$/.exec(line);
    if (fence) {
      const lang = fence[1].trim();
      const body = [];
      i++;
      while (i < lines.length && !/^```/.test(lines[i])) body.push(lines[i++]);
      i++; // closing fence or EOF
      frag.append(codeBlock(body.join("\n"), lang));
      continue;
    }

    // GitHub-style table: a pipe-separated header followed by a delimiter
    // row. Alignment colons are reflected in both header and body cells.
    const table = tableDefinition(lines, i);
    if (table) {
      const rendered = tableElement(lines, i, table);
      frag.append(rendered.element);
      i = rendered.next;
      continue;
    }

    // ATX headings (# … ######), rendered one level down to sit under h1s.
    const head = /^(#{1,6})\s+(.*)$/.exec(line);
    if (head) {
      const h = document.createElement("h" + Math.min(6, head[1].length + 1));
      h.className = "md-h";
      h.append(renderInline(head[2].trimEnd()));
      frag.append(h);
      i++;
      continue;
    }

    if (horizontalRule(line)) {
      frag.append(document.createElement("hr"));
      i++;
      continue;
    }

    // Blockquote: consecutive "> …" lines.
    if (/^>/.test(line)) {
      const quoted = [];
      while (i < lines.length && /^>/.test(lines[i])) {
        quoted.push(lines[i].replace(/^>\s?/, ""));
        i++;
      }
      const bq = document.createElement("blockquote");
      bq.append(renderInline(quoted.join("\n")));
      frag.append(bq);
      continue;
    }

    const marker = listMarker(line);
    if (marker) {
      const list = listElement(lines, i, marker.indent, marker.ordered);
      frag.append(list.element);
      i = list.next;
      continue;
    }

    // Blank line.
    if (line.trim() === "") {
      i++;
      continue;
    }

    // Paragraph: consecutive plain lines, soft line breaks kept.
    const para = [];
    while (i < lines.length) {
      const l = lines[i];
      if (
        l.trim() === "" ||
        /^```/.test(l) ||
        /^#{1,6}\s/.test(l) ||
        /^>/.test(l) ||
        listMarker(l) ||
        horizontalRule(l) ||
        tableDefinition(lines, i)
      ) {
        break;
      }
      para.push(l);
      i++;
    }
    const p = document.createElement("p");
    para.forEach((l, idx) => {
      if (idx > 0) p.append(document.createElement("br"));
      p.append(renderInline(l));
    });
    frag.append(p);
  }

  return frag;
}
