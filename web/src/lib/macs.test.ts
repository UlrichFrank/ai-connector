// pnpm test: node --test (Node strips the types; no test framework needed).
import assert from "node:assert/strict";
import { test } from "node:test";
import type { Job, Model, ModelWorker, Worker } from "../api/types";
import { compareVersions, connectedMacs, fitsNowhere, isOutdated, macsOf, manyMacs, noFitReason, rankWorkers, referenceVersion, specParts, whereText } from "./macs.ts";

const job = (id: string, worker?: string): Job => ({
  id,
  model: "m",
  name: "M",
  kind: "image",
  request: { model: "m" },
  state: "done",
  created: "2026-10-04T10:00:00Z",
  ...(worker ? { worker: { id: worker, name: worker === "w1" ? "Mac Studio" : "MacBook" } } : {}),
});

const mw = (id: string, extra: Partial<ModelWorker> = {}): ModelWorker => ({ id, name: id, here: true, loaded: false, fits: true, ...extra });
const model = (workers?: ModelWorker[]): Model => ({ id: "m", name: "M", kind: "chat", backend: "llm", about: "", size: 1, here: true, where: "", gated: false, defaults: {}, workers });

test("connectedMacs: the hub's list, else the one Mac of the older fields, else none", () => {
  assert.deepEqual(connectedMacs(null), []);
  assert.deepEqual(connectedMacs({ connected: false, name: "", since: "" }), []);
  assert.deepEqual(connectedMacs({ connected: true, id: "w1", name: "Studio", since: "x" }), [{ id: "w1", name: "Studio", since: "x" }]);
  const list = [
    { id: "w1", name: "Studio" },
    { id: "w2", name: "Air" },
  ];
  assert.deepEqual(connectedMacs({ connected: true, id: "w1", name: "Studio", since: "", workers: list }), list);
});

test("manyMacs: several connected, or jobs from more than one Mac; one Mac never", () => {
  assert.equal(manyMacs([], {}), false);
  assert.equal(manyMacs([{ id: "w1", name: "a" }], { a: job("a", "w1"), b: job("b") }), false);
  assert.equal(manyMacs([{ id: "w1", name: "a" }, { id: "w2", name: "b" }], {}), true);
  assert.equal(manyMacs([{ id: "w1", name: "a" }], { a: job("a", "w2") }), true);
  assert.equal(manyMacs([], { a: job("a", "w1"), b: job("b", "w2") }), true);
});

test("macsOf: each Mac once with its count, by name", () => {
  assert.deepEqual(macsOf([job("a", "w2"), job("b", "w1"), job("c", "w2"), job("d")]), [
    { id: "w1", name: "Mac Studio", count: 1 },
    { id: "w2", name: "MacBook", count: 2 },
  ]);
});

test("compareVersions: numeric parts, v prefix and suffixes; unreadable counts as equal", () => {
  assert.equal(compareVersions("1.4.2", "1.10.0"), -1);
  assert.equal(compareVersions("v2.0", "1.9.9"), 1);
  assert.equal(compareVersions("1.4", "1.4.0"), 0);
  assert.equal(compareVersions("1.4.2-3-gabc", "1.4.2"), 0);
  assert.equal(compareVersions("dev", "1.0"), 0);
  assert.equal(isOutdated("1.3.0", "1.4.0"), true);
  assert.equal(isOutdated("1.4.0", undefined), false);
});

test("referenceVersion: the hub's, else the newest a Mac has", () => {
  const w = (v?: string): Worker => ({ id: v ?? "x", name: "", created: "", connected: true, specs: { version: v } });
  assert.equal(referenceVersion("1.5.0", [w("1.6.0")]), "1.5.0");
  assert.equal(referenceVersion(undefined, [w("1.4.0"), w("1.6.1"), w("dev")]), "1.6.1");
  assert.equal(referenceVersion("", []), undefined);
});

test("where a model runs: loaded, on disk, download; too little memory last and disables it", () => {
  assert.equal(whereText(mw("a", { loaded: true })), "loaded");
  assert.equal(whereText(mw("a")), "on disk");
  assert.equal(whereText(mw("a", { here: false })), "downloads first");
  assert.equal(whereText(mw("a", { fits: false, loaded: true })), "too little memory");
  assert.deepEqual(
    rankWorkers([mw("small", { fits: false }), mw("dl", { here: false }), mw("disk"), mw("ram", { loaded: true })]).map((w) => w.id),
    ["ram", "disk", "dl", "small"],
  );
  assert.equal(fitsNowhere(model()), false);
  assert.equal(fitsNowhere(model([])), false);
  assert.equal(fitsNowhere(model([mw("a", { fits: false }), mw("b")])), false);
  assert.equal(fitsNowhere(model([mw("a", { fits: false }), mw("b", { fits: false })])), true);
  assert.equal(noFitReason(model([mw("MacBook", { fits: false })])), "Too big for MacBook's memory");
  assert.equal(noFitReason(model([mw("a")])), "");
});

test("specParts: the known specs in order", () => {
  const w: Worker = { id: "w", name: "", created: "", connected: true, specs: { chip: "Apple M2 Ultra", ramGB: 64, gpuCores: 60, macOS: "27.0.1" } };
  assert.deepEqual(specParts(w), ["Apple M2 Ultra", "64 GB", "60-core GPU", "macOS 27.0.1"]);
  assert.deepEqual(specParts({ ...w, specs: { ramGB: 16 } }), ["16 GB"]);
});
