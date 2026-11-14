import katex from "/assets/vendor/katex-0.19.1/katex.mjs";

// Only KaTeX builds math markup. Model-provided HTML is always plain text.
export function renderMath(source, display, literal, block = display) {
  const node = document.createElement(block ? "div" : "span");
  node.className = display ? "md-math-display" : "md-math-inline";
  try {
    // Bound work and layout for untrusted model output; keep unsupported or
    // incomplete expressions readable rather than dropping the whole reply.
    if (source.length > 20000) throw new Error("Formula too long");
    katex.render(source.trim(), node, {
      displayMode: display,
      throwOnError: true,
      trust: false,
      strict: "error",
      maxExpand: 1000,
      maxSize: 20,
    });
  } catch {
    node.textContent = literal;
    node.classList.add("md-math-fallback");
  }
  return node;
}
