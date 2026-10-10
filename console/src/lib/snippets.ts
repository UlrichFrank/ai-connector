// Clients → usage: copy-paste snippets with this server's URL (and a fresh token) filled in (docs/api.md).
// Pure: tested with `pnpm test` (src/lib/snippets.test.ts).
import { serverUrl, shq } from "./install.ts";

/** Stands in for the token when none was just created (tokens are shown only once). */
export const TOKEN_PLACEHOLDER = "aic_YOUR_TOKEN";
export const DEFAULT_CHAT_MODEL = "qwen3.6-35b-a3b";
export const DEFAULT_EMBED_MODEL = "qwen3-embedding-0.6b";

export interface SnippetInput {
  /** The server's origin ("https://ai.example.com"). */
  origin: string;
  /** A token just created; else the placeholder. */
  token?: string;
  chatModel?: string;
  embedModel?: string;
}

export type SnippetGroup = "openai" | "apps";

export interface Snippet {
  id: string;
  label: string;
  group: SnippetGroup;
  /** What it is written in ("shell", "python", "env"). */
  lang: "shell" | "python" | "env";
  /** One line about where it goes. */
  hint: string;
  text: string;
}

function fill(i: SnippetInput) {
  return {
    server: serverUrl(i.origin),
    token: i.token?.trim() || TOKEN_PLACEHOLDER,
    chat: i.chatModel?.trim() || DEFAULT_CHAT_MODEL,
    embed: i.embedModel?.trim() || DEFAULT_EMBED_MODEL,
  };
}

/** A Python string literal (JSON's escaping is valid Python for these). */
const py = (s: string) => JSON.stringify(s);

export function curlChat(i: SnippetInput): string {
  const { server, token, chat } = fill(i);
  const body = JSON.stringify({ model: chat, messages: [{ role: "user", content: "Hello! Who are you?" }] });
  return [`curl ${shq(`${server}/v1/chat/completions`)}`, `-H ${shq(`Authorization: Bearer ${token}`)}`, `-H 'Content-Type: application/json'`, `-d ${shq(body)}`].join(" \\\n  ");
}

export function curlEmbeddings(i: SnippetInput): string {
  const { server, token, embed } = fill(i);
  const body = JSON.stringify({ model: embed, input: ["The first text", "The second text"] });
  return [`curl ${shq(`${server}/v1/embeddings`)}`, `-H ${shq(`Authorization: Bearer ${token}`)}`, `-H 'Content-Type: application/json'`, `-d ${shq(body)}`].join(" \\\n  ");
}

export function pythonOpenAI(i: SnippetInput): string {
  const { server, token, chat, embed } = fill(i);
  return [
    "from openai import OpenAI  # pip install openai",
    "",
    `client = OpenAI(base_url=${py(`${server}/v1`)}, api_key=${py(token)})`,
    "",
    "chat = client.chat.completions.create(",
    `    model=${py(chat)},`,
    `    messages=[{"role": "user", "content": "Hello! Who are you?"}],`,
    ")",
    "print(chat.choices[0].message.content)",
    "",
    `emb = client.embeddings.create(model=${py(embed)}, input=["The first text", "The second text"])`,
    "print(len(emb.data), len(emb.data[0].embedding))",
  ].join("\n");
}

/**
 * vboard: its environment reads the embedder (TEI, /embed), the classifier (Laya, /v1/systemone) and the cloud AI's
 * endpoint and key (CLOUD_AI_BASE_URL, CLOUD_AI_API_KEY). Provider, model and "enabled" are not environment: an
 * admin picks them on vboard's admin page "Cloud AI" (the comment lines say what to choose).
 */
export function vboardEnv(i: SnippetInput): string {
  const { server, token, chat } = fill(i);
  return [
    `EMBED_URL=${server}`,
    `EMBED_TOKEN=${token}`,
    `CLASSIFIER_URL=${server}`,
    `CLASSIFIER_TOKEN=${token}`,
    `CLOUD_AI_BASE_URL=${server}/v1`,
    `CLOUD_AI_API_KEY=${token}`,
    `# then on vboard's admin page "Cloud AI": provider "OpenAI-compatible",`,
    `#   base URL ${server}/v1, model ${chat}, enabled`,
    `# claude-code/sonnet, claude-code/opus, claude-code/haiku: Claude Code on your own workers,`,
    `#   with your own Claude subscription (the token's owner's workers)`,
  ].join("\n");
}

/** docscanner's backend. */
export function docscannerEnv(i: SnippetInput): string {
  const { server, token } = fill(i);
  return [`AI_CONNECTOR_URL=${server}`, `AI_CONNECTOR_TOKEN=${token}`].join("\n");
}

export function snippets(i: SnippetInput): Snippet[] {
  return [
    { id: "curl-chat", label: "curl · chat", group: "openai", lang: "shell", hint: "POST /v1/chat/completions (add \"stream\": true for SSE)", text: curlChat(i) },
    { id: "curl-embeddings", label: "curl · embeddings", group: "openai", lang: "shell", hint: "POST /v1/embeddings (L2-normalised vectors)", text: curlEmbeddings(i) },
    { id: "python", label: "Python (openai)", group: "openai", lang: "python", hint: "The OpenAI SDK, pointed at this server", text: pythonOpenAI(i) },
    { id: "vboard", label: "vboard sidecar", group: "apps", lang: "env", hint: "vboard's environment (embedder, classifier, cloud AI endpoint and key); provider and model on its admin page", text: vboardEnv(i) },
    { id: "docscanner", label: "docscanner", group: "apps", lang: "env", hint: "docscanner backend's environment", text: docscannerEnv(i) },
  ];
}

/**
 * The model a snippet should name: the first of that kind the server lists — for chat one that is not Claude Code (it
 * runs only for its owner, on their subscription), unless there is nothing else — else the default.
 */
export function pickModel(models: { id: string; kind: string }[], kind: "chat" | "embed", fallback: string): string {
  const of = models.filter((m) => m.kind === kind);
  return (of.find((m) => !m.id.startsWith("claude-code/")) ?? of[0])?.id ?? fallback;
}
