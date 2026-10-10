/// <reference types="node" />
import assert from "node:assert/strict";
import { test } from "node:test";
import { archLabel, loadText, loadedModels, osLabel, platformOf, rankWorkers, specLine, specParts, whereText } from "./specs.ts";

test("platform: from os, an older Mac from macOS", () => {
  assert.equal(platformOf({ os: "darwin" }), "mac");
  assert.equal(platformOf({ macOS: "26.1" }), "mac");
  assert.equal(platformOf({ os: "linux" }), "linux");
  assert.equal(platformOf({ os: "Windows" }), "windows");
  assert.equal(platformOf(undefined), "unknown");
});

test("osLabel", () => {
  assert.equal(osLabel({ macOS: "27.0.1" }), "macOS 27.0.1");
  assert.equal(osLabel({ os: "darwin", osVersion: "27.0" }), "macOS 27.0");
  assert.equal(osLabel({ os: "linux", osVersion: "Ubuntu 24.04" }), "Linux Ubuntu 24.04");
  assert.equal(osLabel({ os: "windows", osVersion: "Windows 11 Pro" }), "Windows 11 Pro");
  assert.equal(osLabel({ os: "linux" }), "Linux");
  assert.equal(osLabel({ os: "freebsd", osVersion: "14" }), "freebsd 14");
  assert.equal(osLabel(undefined), "");
});

test("archLabel", () => {
  assert.equal(archLabel("amd64"), "x86-64");
  assert.equal(archLabel("x86_64"), "x86-64");
  assert.equal(archLabel("aarch64"), "arm64");
  assert.equal(archLabel(undefined), "");
});

test("specParts: a Mac, a Linux PC with a GPU, nothing", () => {
  assert.deepEqual(specParts({ chip: "Apple M2 Ultra", ramGB: 192, gpuCores: 76, macOS: "27.0.1", os: "darwin", arch: "arm64" }), ["Apple M2 Ultra", "192 GB RAM", "76-core GPU", "macOS 27.0.1", "arm64"]);
  assert.deepEqual(specParts({ chip: "AMD Ryzen 9 7950X", ramGB: 64, os: "linux", osVersion: "Debian 13", arch: "amd64", gpu: "NVIDIA RTX 4090", vramGB: 24 }), [
    "AMD Ryzen 9 7950X",
    "64 GB RAM",
    "NVIDIA RTX 4090 (24 GB)",
    "Linux Debian 13",
    "x86-64",
  ]);
  assert.deepEqual(specParts({ vramGB: 8 }), ["8 GB VRAM"]);
  assert.deepEqual(specParts(undefined), []);
  assert.equal(specLine({ specs: { chip: "X", ramGB: 8 } }), "X · 8 GB RAM");
});

test("where a model is on a worker, and the order", () => {
  const w = (here: boolean, loaded: boolean, fits: boolean, id = "") => ({ id, name: id, here, loaded, fits });
  assert.equal(whereText(w(true, true, true)), "loaded");
  assert.equal(whereText(w(true, false, true)), "on disk");
  assert.equal(whereText(w(false, false, true)), "downloads first");
  assert.equal(whereText(w(true, true, false)), "too little memory");
  assert.deepEqual(
    rankWorkers([w(false, false, false, "a"), w(false, false, true, "b"), w(true, false, true, "c"), w(true, true, true, "d")]).map((x) => x.id),
    ["d", "c", "b", "a"],
  );
});

test("loadText and loadedModels", () => {
  assert.equal(loadText({ connected: false }), "offline");
  assert.equal(loadText({ connected: true }), "connected");
  assert.equal(loadText({ connected: true, state: { running: null, queued: 0 } }), "idle");
  assert.equal(loadText({ connected: true, state: { running: "j1", queued: 2 } }), "running · 2 queued");
  assert.deepEqual(loadedModels({ state: { loaded: { mlx: "qwen", go: "" } } }), ["qwen"]);
});
