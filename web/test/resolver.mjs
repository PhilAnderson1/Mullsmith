// Module resolution hook for the lifecycle suite: app.js and its siblings
// import each other by ABSOLUTE /assets/app/*.js URLs because the server's
// commit-stamp import rewriter recognises that form. Node cannot resolve
// those URLs on its own, so this hook maps
// them into web/assets/app/ — the same graph, no production compromise.
export function resolve(specifier, context, nextResolve) {
  if (specifier.startsWith("/assets/app/") || specifier.startsWith("/assets/vendor/")) {
    return {
      url: new URL("../" + specifier.slice(1), import.meta.url).href,
      format: "module",
      shortCircuit: true,
    };
  }
  return nextResolve(specifier, context);
}
