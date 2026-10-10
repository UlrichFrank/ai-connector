import assert from "node:assert/strict";
import { test } from "node:test";
import { routedFile, routeOfJob } from "./files.ts";

test("routeOfJob", () => {
  assert.equal(routeOfJob("m2.153000-4"), "m2");
  assert.equal(routeOfJob("153000-4"), undefined);
  assert.equal(routeOfJob(undefined), undefined);
  assert.equal(routeOfJob("M2.x"), undefined);
});

test("routedFile", () => {
  assert.equal(routedFile("/files/tools/r.pdf", "m1"), "/files/m1/tools/r.pdf");
  assert.equal(routedFile("/files/m1/tools/r.pdf", "m1"), "/files/m1/tools/r.pdf");
  assert.equal(routedFile("/files/x.png", undefined), "/files/x.png");
  assert.equal(routedFile("https://example.org/files/x", "m1"), "https://example.org/files/x");
});
