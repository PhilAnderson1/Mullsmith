import test from "node:test";
import assert from "node:assert/strict";

import { performanceText } from "../assets/app/performance.js";

test("performance footer separates answer latency from active generation rate", () => {
  assert.equal(performanceText(4826, 70, 10000), "4.8s to answer · 7.0 tok/s");
  assert.equal(performanceText(275, 0, 0), "275ms to answer");
  assert.equal(performanceText(0, 700, 1000), "700 tok/s");
  assert.equal(performanceText(0, 0, 0), "");
});
