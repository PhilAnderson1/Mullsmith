// Markdown → DOM renderer for model output (spec §9.4). XSS-safe by
// construction: output is built exclusively with createElement/textContent,
// never innerHTML, so untrusted model text can never introduce markup.
// Supported: fenced code (+copy), # headings, **bold**, *italic*, `code`,
// [links](http/https only), - / * / 1. lists, > quotes. Anything else is
// literal text.

const INLINE_RE =
  /`([^`\n]+)`|\*\*([^*\n]+)\*\*|\*([^*\n]+)\*|\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)/g;

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
      const b = document.createElement("strong");
      b.textContent = m[2];
      frag.append(b);
    } else if (m[3] !== undefined) {
      const em = document.createElement("em");
      em.textContent = m[3];
      frag.append(em);
    } else {
      const a = document.createElement("a");
      a.href = m[5]; // regex admits only http(s) URLs
      a.textContent = m[4];
      a.rel = "noopener noreferrer nofollow";
      a.target = "_blank";
      frag.append(a);
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

    // ATX headings (# … ####), rendered one level down to sit under h1s.
    const head = /^(#{1,4})\s+(.*)$/.exec(line);
    if (head) {
      const h = document.createElement("h" + Math.min(6, head[1].length + 1));
      h.className = "md-h";
      h.append(renderInline(head[2].trimEnd()));
      frag.append(h);
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

    // Lists: one item per line while the marker repeats.
    const ulMark = /^\s*[-*+]\s+(.*)$/;
    const olMark = /^\s*\d+[.)]\s+(.*)$/;
    if (ulMark.test(line) || olMark.test(line)) {
      const ordered = olMark.test(line);
      const mark = ordered ? olMark : ulMark;
      const list = document.createElement(ordered ? "ol" : "ul");
      while (i < lines.length) {
        const m = mark.exec(lines[i]);
        if (!m) break;
        const li = document.createElement("li");
        li.append(renderInline(m[1]));
        list.append(li);
        i++;
      }
      frag.append(list);
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
        /^#{1,4}\s/.test(l) ||
        /^>/.test(l) ||
        ulMark.test(l) ||
        olMark.test(l)
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
