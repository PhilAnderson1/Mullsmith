export function performanceText(firstAnswerMS, completion, durationMS) {
  let answer = "";
  if (firstAnswerMS > 0) {
    const elapsed = firstAnswerMS < 1000
      ? `${Math.round(firstAnswerMS)}ms`
      : `${(firstAnswerMS / 1000).toFixed(1)}s`;
    answer = `${elapsed} to answer`;
  }
  let rate = "";
  if (completion > 0 && durationMS > 0) {
    const tps = (completion / durationMS) * 1000;
    rate = `${tps >= 100 ? Math.round(tps) : tps.toFixed(1)} tok/s`;
  }
  return [answer, rate].filter(Boolean).join(" · ");
}
