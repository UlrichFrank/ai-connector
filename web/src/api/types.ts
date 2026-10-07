// The server's JSON, as the Go types write it (internal/engine, internal/jobs, internal/prompts).

export type Kind = "chat" | "embed" | "classify" | "image" | "video" | "music" | "speech" | "avatar" | "sound";
export type JobState = "queued" | "running" | "done" | "failed" | "canceled";

/** A chat message. The Go type has no JSON tags: the keys are capitalised. */
export interface Message {
  Role: "system" | "user" | "assistant" | "tool";
  Content: string;
  /** An assistant turn that called tools (chat with tools, docs/chat-tools.md). */
  ToolCalls?: ToolCall[];
  /** A tool turn: the call it answers. */
  ToolCallID?: string;
}

// ---------- chat with tools (internal/agent/contract.go, docs/chat-tools.md) ----------

/** What a chat may touch, fixed when it starts: "" = no tools. */
export type Zone = "" | "home" | "web" | "files";

/** A call in an assistant turn of the history (`result.messages`). */
export interface ToolCall {
  ID?: string;
  Name: string;
  Arguments?: unknown;
}

export interface Source {
  title: string;
  url: string;
}

/** The guard's word: screen "ok" | "injection" | "irrelevant"; grade "good" | "bad" | "unsure". */
export interface Verdict {
  label: string;
  score: number;
  why?: string;
}

export type ToolState = "approval" | "running" | "done" | "error" | "denied" | "blocked" | "held";

/** The data of an engine event of type "tool": one per change of a call's state (keyed by `id`). */
export interface ToolEvent {
  id: string;
  name: string;
  zone: Zone;
  arguments?: unknown;
  state: ToolState;
  result?: string;
  sources?: Source[];
  files?: string[];
  verdict?: Verdict;
  /** While waiting for approval: when it counts as denied (RFC 3339), and what the tool does. */
  deadline?: string;
  description?: string;
  /** It changes something but ran without asking: the user allowed the tool always (Settings → Tools). */
  auto?: boolean;
  /** Kept on the job: the call waited for the user's approval (it was approved unless denied). */
  asked?: boolean;
}

/** A home tool (GET /api/tools). */
export interface HomeTool {
  name: string;
  description?: string;
  /** It changes something: it asks before each call unless `always`. Read-only tools never ask. */
  changes: boolean;
  always: boolean;
}

/** An MCP server of the home zone and its tools; `error` when it did not answer. */
export interface ToolServer {
  name: string;
  error?: string;
  tools: HomeTool[];
}

/** GET /api/tools. */
export interface ToolsResponse {
  servers: ToolServer[];
  /** The tools allowed always (approvals.json), also those of servers that are not there now. */
  always: string[];
  /** The servers could not be listed at all (mcp.json unreadable). */
  error?: string;
}

export interface Clip {
  prompt: string;
  frames?: number;
  continue?: boolean;
}

export interface Option {
  label: string;
  description?: string;
}

export interface Request {
  model: string;
  // chat
  messages?: Message[];
  /** Chat with tools: what the chat may touch (kept by every follow-up turn). */
  zone?: Zone;
  /** Unset: the model's default for thinking on or off (Model.sampling); 0 means unset — greedy is its own flag. */
  temperature?: number;
  topP?: number;
  topK?: number;
  presencePenalty?: number;
  /** Always the most likely token (temperature 0). */
  greedy?: boolean;
  /** Unset: 32768 with thinking, else 4096. */
  maxTokens?: number;
  think?: boolean;
  // embed, classify
  texts?: string[];
  states?: string[];
  instructions?: string;
  options?: Option[];
  // image, video
  prompt?: string;
  width?: number;
  height?: number;
  steps?: number;
  seed?: number;
  guidance?: number;
  quantize?: number;
  images?: string[];
  frames?: number;
  fps?: number;
  lowRam?: boolean;
  script?: Clip[];
  noAudio?: boolean;
  // music
  lyrics?: string;
  duration?: number;
  instrumental?: boolean;
  /** the tempo (0/none: "NNN bpm" in the style, else the model's choice) */
  bpm?: number;
  /** stretch a new song to its tempo when it is off by ≤ 12 % (default on when there is a tempo) */
  lockTempo?: boolean;
  /** best of N (1–3): the take closest to the tempo is kept */
  takes?: number;
  /** ACE-Step's planner LM: "" 1.7B, "4b", "off" */
  planner?: string;
  /** ACE-Step's timestep shift (0/none: 3.0 turbo, 1.0 base; a style's recommendation first) */
  shift?: number;
  title?: string;
  folder?: string;
  coverFor?: string;
  task?: "generate" | "cover" | "repaint" | "extend" | "lego" | "extract" | "restyle" | "train-style";
  source?: string;
  parent?: string;
  start?: number;
  end?: number;
  strength?: number;
  track?: string;
  extend?: number;
  abc?: string;
  /** A trained style (GET /api/styles) of the model; its trigger word is added to the prompt. Task "train-style" trains it. */
  style?: string;
  /** The style's LoRA scale (0..2; unset: the style's own strength). */
  styleStrength?: number;
  // speech (docs/voice-avatar.md): the text to say, a library voice id or a model preset name, how it should sound
  text?: string;
  voice?: string;
  instruct?: string;
  /** "auto", "de", "en", … */
  language?: string;
  speed?: number;
  // avatar: `images[0]` the portrait; `audio` (a /files/ url) or `text` + `voice` + `speechModel`; `prompt` the scene.
  // speech with the model qwen3-asr: `audio` → the job's `text` (a transcription).
  audio?: string;
  speechModel?: string;
  /** The hub: the Mac (worker id) to run on; unset lets the hub pick (docs/architecture.md, "Several Macs"). */
  worker?: string;
}

/** A chat model's sampling settings (internal/llm Options). */
export interface Sampling {
  maxTokens: number;
  temperature: number;
  topK: number;
  topP: number;
  minP: number;
  presencePenalty: number;
  repetitionPenalty: number;
}

export interface Model {
  id: string;
  name: string;
  kind: Kind;
  backend: string;
  about: string;
  size: number;
  here: boolean;
  where: string;
  gated: boolean;
  tasks?: string[];
  /** A song model that can learn a style from your tracks (and take one: Request.style). */
  styles?: boolean;
  defaults: Partial<Request>;
  /** A chat model's sampling with thinking on and off: what a request gets for the fields it leaves unset. */
  sampling?: { think: Sampling; noThink: Sampling };
  /** "Apache-2.0", "CC BY-NC 4.0", … */
  licence?: string;
  /** Not for commercial use (the pickers show a badge). */
  nonCommercial?: boolean;
  /** The hub with several Macs: where it can run — each connected Mac, whether it has it, holds it, has the memory. */
  workers?: ModelWorker[];
}

/** One connected Mac as a model sees it (GET /api/models on the hub). */
export interface ModelWorker {
  id: string;
  name: string;
  /** On its disk (else it would download first). */
  here: boolean;
  /** In its memory now. */
  loaded: boolean;
  /** Its RAM is enough for the model. */
  fits: boolean;
}

// ---------- the voice library (internal/engine/voice_contract.go, docs/voice-avatar.md) ----------

/** A voice of the library: cloned from a sample of the speaker, or designed from a description. */
export interface Voice {
  id: string;
  name: string;
  kind: "clone" | "design";
  /** clone: the sample (/files/ url). */
  sample?: string;
  /** clone: what the sample says. */
  transcript?: string;
  /** design: "a calm older woman, low voice, slight Bavarian accent". */
  description?: string;
  language?: string;
  created: string;
}

/** GET /api/voices: the library, and each model's own named voices. */
export interface VoicesResponse {
  voices: Voice[];
  presets?: Record<string, string[]>;
}

export interface Example {
  title: string;
  prompt?: string;
  size?: string;
  frames?: number;
  script?: Clip[];
  lyrics?: string;
  duration?: number;
  texts?: string[];
  question?: string;
  [k: string]: unknown;
}

export interface Problem {
  title: string;
  hint?: string;
}

export interface Job {
  id: string;
  model: string;
  name: string;
  kind: Kind;
  request: Request;
  state: JobState;
  created: string;
  started?: string;
  finished?: string;
  status?: string;
  text?: string;
  step?: number;
  total?: number;
  files?: string[];
  preview?: string;
  cover?: string;
  starred?: boolean;
  stats?: Record<string, unknown>;
  result?: Record<string, unknown>;
  logs?: string[];
  error?: string;
  problem?: Problem;
  score?: string;
  /** Chat with tools: the calls' last states, if the server keeps them on the job. */
  tools?: ToolEvent[];
  /** The hub: the Mac that holds the job. */
  worker?: { id: string; name: string };
}

export interface EngineEvent {
  type: "status" | "log" | "text" | "progress" | "file" | "preview" | "stats" | "result" | "tool";
  text?: string;
  step?: number;
  total?: number;
  file?: string;
  data?: Record<string, unknown>;
}

export interface Update {
  jobId: string;
  job?: Job;
  event?: EngineEvent;
}

export type PromptUse = "chat" | "enhance-image" | "enhance-video" | "enhance-music" | "write-song" | "cover-art";

export interface Prompt {
  id: string;
  name: string;
  use: PromptUse;
  text: string;
  source?: string;
  builtin: boolean;
  default?: string;
  modified: boolean;
  updated?: string;
}

export interface ModelsResponse {
  models: Model[];
  loaded: Record<string, string>;
  kinds: Kind[];
  examples: Partial<Record<Kind, Example[]>>;
}

// ---------- the hub (docs/architecture.md) ----------

/** Who serves the web app: the hub (signs people in, forwards to the Mac) or `mlxlab serve` on the Mac itself. */
export type Role = "hub" | "worker";

/** The Mac that does the work, as the hub sees it (GET /api/worker/events, /api/info). */
export interface WorkerState {
  connected: boolean;
  /** The enrolled worker (GET /api/workers) on the tunnel; absent for the legacy shared token. */
  id?: string;
  name: string;
  /** When it connected or went away (RFC 3339). */
  since: string;
  /** Several Macs: every connected one (the fields above describe one of them, for older clients). */
  workers?: ConnectedMac[];
}

/** A Mac on the tunnel now (GET /api/worker/events `workers`). */
export interface ConnectedMac {
  id: string;
  name: string;
  since?: string;
}

/** GET /api/info (no sign-in needed). */
export interface Info {
  app: string;
  role: Role;
  name: string;
  signedIn: boolean;
  /** Only from the hub. */
  worker?: WorkerState;
  /** The hub: who uses this device ("Philipp"; "" for a device from before user names, or signed out). */
  user?: string;
  /** The hub: this device signed in with the password (it may change other devices). */
  owner?: boolean;
  /** The hub: this device may show a QR code that signs in an iPhone (always true for an owner). */
  canInvite?: boolean;
  /** The hub's mlxlab version, if it says (a Mac with an older one is flagged on Devices). */
  version?: string;
}

/** A browser or phone that signed in to the hub. */
export interface Device {
  id: string;
  name: string;
  /** Who uses it ("Philipp"); "" for a device signed in before user names existed. */
  user?: string;
  created: string;
  lastSeen: string;
  /** The device this request came from. */
  current?: boolean;
  /** Signed in with the password: may invite, and may change other devices. */
  owner?: boolean;
  /** May show a QR code that signs in another device (always true for an owner). */
  canInvite?: boolean;
  /** The id of the device whose QR code signed this one in ("" for an owner); it may be gone since. */
  invitedBy?: string;
}

/** POST /api/invites: a one-time code that signs in one iPhone; the QR code holds `url`. */
export interface Invite {
  code: string;
  /** mlxlab://signin?hub=…&code=… */
  url: string;
  /** RFC 3339; 10 minutes after it was made. */
  expires: string;
}

/** A Mac enrolled with its own token (GET /api/workers). */
export interface Worker {
  id: string;
  name: string;
  created: string;
  /** When it was last connected (RFC 3339; empty before it ever connected). */
  lastSeen?: string;
  /** This one is on the tunnel now. */
  connected: boolean;
  /** What it is (from its /api/info; absent before it ever connected, or from an older hub). */
  specs?: WorkerSpecs;
  /** What it does now. */
  state?: WorkerLoad;
  /** Serves the per-Mac libraries (prompts, tools, …): the one with the most RAM unless pinned. */
  primary?: boolean;
  /** The primary because someone chose it (PUT …/primary); DELETE unpins. */
  pinned?: boolean;
  /** Its short route id ("m2"): the prefix of its job ids and /files/ urls. */
  route?: string;
  /** It understands route ids (an older mlxlab does not: update it). */
  routes?: boolean;
  /** The Mac with the old shared token (not enrolled; it cannot be revoked here). */
  legacy?: boolean;
  /** When it connected (RFC 3339). */
  since?: string;
}

export interface WorkerSpecs {
  /** "Apple M2 Ultra" */
  chip?: string;
  ramGB?: number;
  gpuCores?: number;
  /** "27.0.1" */
  macOS?: string;
  /** Its mlxlab version. */
  version?: string;
}

export interface WorkerLoad {
  /** The job id it runs, or null. */
  running?: string | null;
  queued?: number;
  /** backend → model id */
  loaded?: Record<string, string>;
}

/** POST /api/workers: the token is shown this once. */
export interface WorkerCreated {
  worker: Worker;
  token: string;
}

/** POST /api/login. The hub also sets the session cookie, so the browser does not need the token. */
export interface LoginResponse {
  token: string;
  device: Device;
}

/** The body of an error answer: `{"error":"signin"}`, `{"error":"offline","message":"The Mac is offline"}`. */
export interface ErrorBody {
  error: string;
  message?: string;
}

// ---------- trained styles (GET /api/styles; docs/architecture.md, "Train your own style") ----------

export type StyleStatus = "new" | "ready" | "queued" | "training" | "paused" | "trained" | "failed";

export interface StyleSettings {
  steps: number;
  rank: number;
  alpha: number;
  lr: number;
  warmup: number;
  segmentSeconds: number;
  checkpointEvery: number;
  /** 0: no samples during training. */
  sampleEvery: number;
  sampleSeconds: number;
  samplePrompt?: string;
  seed: number;
}

export interface StyleTrack {
  id: string;
  name: string;
  /** /files/styles/<style>/tracks/<id>.wav */
  url: string;
  seconds: number;
  segments: number;
  vocals?: boolean;
  /** Measured tempo (Stable Audio 3 puts it into the captions). */
  bpm?: number;
  added: string;
}

export interface Style {
  id: string;
  name: string;
  /** The song model it is trained for (and works with). */
  baseModel: string;
  /** Made-up word the captions start with; added to a song's style text. */
  trigger: string;
  /** What the tracks sound like, in tags: every track's caption for a model without a captioner (Stable Audio 3). */
  description?: string;
  /** Default LoRA scale (0..2). */
  strength: number;
  status: StyleStatus;
  error?: string;
  settings: StyleSettings;
  created: string;
  trained?: string;
  /** "lora" when trained. */
  weights?: string;
  tracks: StyleTrack[];
  /** Length of all tracks (s) and the segments they give. */
  seconds: number;
  segments: number;
  /** About how long a training (or what is left of it) takes, in seconds. */
  estimate: number;
  progress: {
    phase?: string;
    step: number;
    total: number;
    loss?: number;
    checkpoint?: number;
    secondsPerStep?: number;
    eta?: number;
    started?: string;
    updated?: string;
  };
  losses: { step: number; loss: number }[];
  samples: { step: number; url: string }[];
  /** What the model heard in each segment (auto-labels). */
  captions?: Record<string, string>;
  /** The last training job; `running` while it is queued or running. */
  jobId?: string;
  running?: Job;
  /** The hub: the Mac that has it (styles stay where they were trained). */
  worker?: { id: string; name: string };
  /** A community style: a LoRA from Hugging Face, pinned (not trained here; it cannot be trained). */
  source?: StyleSource;
  /** The LoRA's weights file (community styles). */
  weightsFile?: string;
  /** Settings the LoRA was made for: a song that leaves them on Auto gets them. */
  recommended?: StyleRecommended;
  /** Prompts the LoRA's author suggests (the trigger word is added anyway). */
  examples?: StyleExample[];
}

export interface StyleSource {
  repo: string;
  rev: string;
  url: string;
  author?: string;
  /** "" or missing: the repository states no licence. */
  licence?: string;
  /** The base model as the repository names it. */
  baseText?: string;
  size?: number;
  rank?: number;
  alpha?: number;
}

export interface StyleRecommended {
  steps?: number;
  guidance?: number;
  shift?: number;
}

export interface StyleExample {
  prompt: string;
  bpm?: number;
  key?: string;
}

/** A LoRA of the curated list (GET /api/styles → community). */
export interface CommunityStyle {
  repo: string;
  rev: string;
  name: string;
  about: string;
  baseModel: string;
  trigger: string;
  licence: string;
  size: number;
}

/** What a LoRA repository says about itself (GET /api/community-styles?repo=…). */
export interface CommunityLookup {
  repo: string;
  rev: string;
  url: string;
  name: string;
  description?: string;
  author?: string;
  licence: string;
  gated?: boolean;
  /** "": the user names it. */
  trigger: string;
  triggers?: string[];
  baseText?: string;
  /** "": not known, the user picks. */
  baseModel: string;
  strength: number;
  tested?: number[];
  recommended: StyleRecommended;
  examples?: StyleExample[];
  weightsFile: string;
  size: number;
  rank: number;
  alpha: number;
  metadata?: string;
  curated?: boolean;
}

export interface StylesResponse {
  styles: Style[];
  /** The curated community LoRAs. */
  community?: CommunityStyle[];
  /** The models styles can be trained for; `described`: its styles need a description (no captioner). */
  models: { id: string; name: string; here: boolean; stepSeconds: number; described?: boolean; defaults?: StyleSettings }[];
  defaults: StyleSettings;
}

