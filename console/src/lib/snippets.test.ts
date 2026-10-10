/// <reference types="node" />
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { curlChat, curlEmbeddings, docscannerEnv, pickModel, pythonOpenAI, snippets, TOKEN_PLACEHOLDER, vboardEnv } from "./snippets.ts";

const input = { origin: "https://ai.example.com/", token: "aic_abc", chatModel: "ollama/qwen3:8b", embedModel: "qwen3-embedding-0.6b" };

test("curl chat: the URL, the token and a JSON body the shell passes through intact", () => {
  const c = curlChat(input);
  assert.ok(c.startsWith("curl 'https://ai.example.com/v1/chat/completions'"));
  assert.ok(c.includes("-H 'Authorization: Bearer aic_abc'"));
  // the shell hands curl the body as one argument: echo it back instead of calling curl
  const body = execFileSync("sh", ["-c", c.replace(/^curl /, "f() { for a; do last=$a; done; printf '%s' \"$last\"; }; f ")], { encoding: "utf8" });
  assert.deepEqual(JSON.parse(body), { model: "ollama/qwen3:8b", messages: [{ role: "user", content: "Hello! Who are you?" }] });
});

test("curl embeddings", () => {
  const c = curlEmbeddings(input);
  assert.ok(c.includes("'https://ai.example.com/v1/embeddings'"));
  assert.ok(c.includes('"model":"qwen3-embedding-0.6b"'));
});

test("python: base_url ends in /v1, models filled in", () => {
  const p = pythonOpenAI(input);
  assert.ok(p.includes('OpenAI(base_url="https://ai.example.com/v1", api_key="aic_abc")'));
  assert.ok(p.includes('model="ollama/qwen3:8b"'));
  assert.ok(p.includes('client.embeddings.create(model="qwen3-embedding-0.6b"'));
});

test("vboard env: the server for TEI and Laya, /v1 for the cloud AI provider", () => {
  const lines = vboardEnv(input).split("\n");
  assert.ok(lines.includes("EMBED_URL=https://ai.example.com"));
  assert.ok(lines.includes("EMBED_TOKEN=aic_abc"));
  assert.ok(lines.includes("CLASSIFIER_URL=https://ai.example.com"));
  assert.ok(lines.includes("CLASSIFIER_TOKEN=aic_abc"));
  assert.ok(lines.includes("CLOUD_AI_PROVIDER=openai"));
  assert.ok(lines.includes("CLOUD_AI_BASE_URL=https://ai.example.com/v1"));
});

test("docscanner env", () => {
  assert.equal(docscannerEnv(input), "AI_CONNECTOR_URL=https://ai.example.com\nAI_CONNECTOR_TOKEN=aic_abc");
});

test("without a token or models: the placeholder and the defaults", () => {
  const all = snippets({ origin: "http://localhost:8080" });
  assert.equal(all.length, 5);
  for (const s of all) assert.ok(s.text.includes(TOKEN_PLACEHOLDER), s.id);
  assert.ok(curlChat({ origin: "http://x" }).includes("qwen3.6-35b-a3b"));
});

test("pickModel: the first of the kind, else the fallback", () => {
  const ms = [
    { id: "flux", kind: "image" },
    { id: "e1", kind: "embed" },
    { id: "c1", kind: "chat" },
    { id: "c2", kind: "chat" },
  ];
  assert.equal(pickModel(ms, "chat", "x"), "c1");
  assert.equal(pickModel(ms, "embed", "x"), "e1");
  assert.equal(pickModel([], "chat", "x"), "x");
});
