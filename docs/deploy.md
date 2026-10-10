# Deploying the server

The server (`cmd/ai-connector-server`, image `pharndt/ai-connector-server`) runs on the Raspberry Pi k0s cluster
next to the mlxlab hub, as **its own Helm release** (`ai-connector` in the namespace `ai-connector`). It is one pure-Go
binary with both web apps embedded (the console at `/console/`, mlxlab's app at `/`) and the worker programs for
macOS, Linux and Windows (served at `/tools/mlxlab/<platform>` for "Connect a worker").

| | |
|---|---|
| Image | `pharndt/ai-connector-server:<tag>`, linux/arm64 + amd64, distroless static, user 65532, ~37 MB compressed |
| Chart | [`deploy/helm/ai-connector`](../deploy/helm/ai-connector) in this repo |
| Release / namespace | `ai-connector` / `ai-connector` (mlxlab's hub stays `mlxlab` / `mlxlab`) |
| Ingress | two host names: `hosts.console` (GET / → /console/) and `hosts.app` (mlxlab's app), traefik, TLS |
| Storage | PVC `ai-connector-data` (1Gi, ReadWriteOnce) at `/data`; kept on uninstall |
| Probes | `GET /healthz` → `200 ok` (no auth, no worker needed) |
| Resources | Guaranteed QoS, 250m / 384Mi, `GOMEMLIMIT=300MiB` |
| Security | non-root 65532, read-only root filesystem (writes only `/data` and an emptyDir `/tmp`), no capabilities |

One replica, `Recreate`: the worker tunnel, the sessions and the JSON stores live in the one process, and the
ReadWriteOnce volume would hold a rolling update up.

## What is in `/data`

Small JSON files, written atomically (temp file + rename):

| File | What | Lost means |
|---|---|---|
| `devices.json` | signed-in devices (tokens hashed), their users, owners, who invited whom | everyone signs in again |
| `workers.json` | enrolled workers (tokens hashed), their route ids and owners (whose Claude Code they run) | every worker is connected again (console → Connect a worker), owners set again |
| `clients.json` | API clients (`aic_…` tokens hashed) and their owners | every app gets a new token |
| `primary.json` | the pinned primary worker (only after a pin) | — (falls back to automatic) |
| `connectors.json` | vboard connectors (address, board, models, owner) **with their vboard tokens in clear** (mode 0600: the server needs them to dial) | the connectors are added again, with new vboard tokens |

These are all the files the server writes there (pending invites live in memory only). Treat a backup as a
secret: `connectors.json` holds tokens that open vboard boards.

## First install on the Pi

Needs, as for mlxlab: `helm` with the [helm-secrets](https://github.com/jkroepke/helm-secrets) plugin and `sops`
(the gitops repo's age key), Rancher Desktop on the Mac (`docker --context rancher-desktop`), ssh to the Pi with
sudo for `k0s ctr`, and the gitops kubeconfig (`$(GITOPS)/kubeconfig`).

### 1. Host names and DNS

Pick two names under the wildcard certificate (`*.example.org`, the `ingress-tls-certs` secret that the reflector copies
into every namespace) and point them at the Pi's traefik like `mlxlab.example.org`:

- `hosts.console`, e.g. `ai.example.org` — the console; `GET /` redirects to `/console/` (`MLXLAB_CONSOLE_HOSTS`).
- `hosts.app` — mlxlab's web app, TUI and iOS app. **While the mlxlab hub runs, this must not be
  `mlxlab.example.org`**: two Ingresses with the same host would make traefik split one name between two servers. Use a
  new name (e.g. `lab.example.org`) or leave it empty (`""`, console host only) until the switch-over below.

Every host serves both apps at their paths, so the console host alone is enough to start.

### 2. Values in the gitops repo

`cluster/charts/apps/ai-connector/values/values.yaml` (next to `apps/mlxlab`; the chart itself stays in this repo):

```yaml
image:
  # imported into the Pi's containerd by `make deploy`, never pulled
  pullPolicy: Never

hosts:
  console: ai.example.org
  app: ""            # e.g. lab.example.org; mlxlab.example.org only after the mlxlab release is gone

models:
  embed: ""          # MLXLAB_EMBED_MODEL, default qwen3-embedding-0.6b
  classify: ""       # MLXLAB_CLASSIFY_MODEL, default laya-typed-decisions

env:
  TZ: "Europe/Berlin"
  GOMEMLIMIT: "300MiB"

ingress:
  className: traefik
  tls:
    secretName: ingress-tls-certs   # the reflected *.example.org certificate

persistence:
  storageClass: local-path
  size: 1Gi
```

`cluster/charts/apps/ai-connector/values/secrets.yaml` (sops; the same shape as mlxlab's), from the repo root so
`.sops.yaml` applies:

```sh
f=cluster/charts/apps/ai-connector/values/secrets.yaml
printf 'secrets:\n    MLXLAB_PASSWORD: "%s"\n    MLXLAB_WORKER_TOKEN: "%s"\n' \
  "<a password>" "$(openssl rand -base64 32)" > "$f"
sops --encrypt --in-place "$f"
```

`MLXLAB_PASSWORD` is required (the server refuses to start without one). `MLXLAB_WORKER_TOKEN` is optional: workers
get their own tokens in the console; the shared token is only for a worker set up by hand. Use a different password
and worker token than mlxlab's: the two servers are separate.

Without sops: create the Secret yourself and name it in the values (`secrets.yaml` is then left out):

```sh
kubectl create namespace ai-connector
kubectl -n ai-connector create secret generic ai-connector-auth \
  --from-literal=MLXLAB_PASSWORD='<a password>' --from-literal=MLXLAB_WORKER_TOKEN="$(openssl rand -base64 32)"
# values.yaml:  existingSecret: ai-connector-auth
```

### 3. Deploy

From this repo (not run by CI):

```sh
make deploy        # build linux/arm64, import into the Pi's containerd over ssh, helm upgrade --install, check
make deploy-status # pods, ingress, PVC, /healthz
```

`make deploy` builds `pharndt/ai-connector-server:<commit>[-dirty-<time>]`, streams it with
`docker save | ssh $(PI) 'sudo k0s ctr -n k8s.io images import -'`, runs
`helm secrets upgrade --install -n ai-connector --create-namespace ai-connector deploy/helm/ai-connector -f
<gitops>/…/values.yaml -f <gitops>/…/secrets.yaml --set image.tag=<that tag> --set image.pullPolicy=Never --wait`,
waits for the rollout (of the Deployment named by the chart's fullname rules for `AIC_RELEASE` and the values — `helm template`; `make chart-lint` checks those rules), removes older `pharndt/ai-connector-server` images from the Pi and curls `/healthz`. It
refuses to run if the release, namespace or image name mentions `mlxlab`, so it cannot touch the mlxlab hub.

The variables, all overridable on the command line:

| Variable | Default | |
|---|---|---|
| `PI` | `user@pi.lan` | ssh target of the k0s node |
| `GITOPS` | `<gitops checkout>` | the gitops checkout (kubeconfig, values) |
| `AIC_VALUES` | `$(GITOPS)/cluster/charts/apps/ai-connector/values` | `values.yaml` and `secrets.yaml` |
| `AIC_RELEASE` / `AIC_NAMESPACE` | `ai-connector` / `ai-connector` | |
| `AIC_URL` | `https://ai.example.org` | checked after the deploy (the console host) |
| `DOCKER` | `docker --context rancher-desktop` | missing base images come through Apple's `container` CLI |
| `KUBE` | `KUBECONFIG=$(GITOPS)/kubeconfig` | |

The same by hand, with a released image from Docker Hub instead (tag `server-v0.1.0` → `0.1.0`):

```sh
export KUBECONFIG=<gitops checkout>/kubeconfig
V=<gitops checkout>/cluster/charts/apps/ai-connector/values
helm secrets upgrade --install -n ai-connector --create-namespace ai-connector deploy/helm/ai-connector \
  -f $V/values.yaml -f $V/secrets.yaml --set image.tag=0.1.0 --set image.pullPolicy=IfNotPresent --wait
```

Check: `curl https://ai.example.org/healthz` → `ok`; `curl https://ai.example.org/api/info` →
`{"app":"mlxlab","role":"hub",…,"workers":[]}`; `https://ai.example.org/` opens the console's sign-in.

## Traefik: WebSocket, SSE, long streams, uploads

The chart needs no traefik annotations. Traefik does not buffer requests or responses unless a buffering middleware
is attached, has no request body limit, flushes `text/event-stream` at once (`/api/events`, `/api/worker/events`,
streamed `/v1/chat/completions`) and passes WebSocket upgrades (`/api/worker/connect`, the workers' tunnel) through.
The server itself sets no read or write timeout.

The one limit is cluster-wide: traefik v3's `respondingTimeouts.readTimeout` on the `websecure` entryPoint is
**60s** by default. It bounds reading a whole request, so a slow large upload fails and the workers' long-lived
WebSocket may be cut (they reconnect, but the tunnel flaps). mlxlab's README in the gitops repo has the fix
(`ports.websecure.transport.respondingTimeouts.readTimeout: 0` in traefik's values); it applies to both servers.

With cert-manager per Ingress instead of the reflected wildcard: `ingress.annotations: {cert-manager.io/cluster-issuer:
<issuer>}` and an empty `ingress.tls.secretName` (cert-manager then fills `ai-connector-tls` for both hosts).

## Upgrading

`make deploy` again (each build gets a new tag, so the pod is recreated; the PVC stays). A Secret change restarts
the pod (checksum annotation); with `existingSecret`, restart it yourself or add the reloader annotation
(`podAnnotations: {reloader.stakater.com/auto: "true"}`). During the restart (a few seconds, `Recreate`) workers
reconnect with backoff and clients get connection errors or `503`.

Rolling back: `make deploy` from the older commit, or `helm rollback -n ai-connector ai-connector` while the older
image is still on the Pi (`make deploy` removes the older ones after a successful rollout).

## Next to the mlxlab hub

The two are separate releases with separate data, passwords and worker tokens:

| | mlxlab hub | ai-connector |
|---|---|---|
| Release / namespace | `mlxlab` / `mlxlab` | `ai-connector` / `ai-connector` |
| Image | `pharndt/mlxlab-hub` | `pharndt/ai-connector-server` |
| Chart | gitops `cluster/charts/apps/mlxlab/chart` | this repo, `deploy/helm/ai-connector` |
| Deploy | `make deploy` in local-ai-tests | `make deploy` here |
| Hosts | `mlxlab.example.org` | `ai.example.org` (+ a second app host) |

A worker serves **one** server at a time: it remembers the server and its token in `~/.config/mlxlab/worker.json`,
which the paste from "Connect a worker" overwrites. So moving a Mac over to ai-connector takes it away from the
mlxlab hub; `ai-connector worker --hub https://mlxlab.example.org --token …` (or mlxlab's `make worker-connect`) moves
it back.

**Switching over** (later, when ai-connector replaces the hub): connect the workers to ai-connector, uninstall the
mlxlab release (`gitops/cluster/charts/apps/mlxlab/uninstall.sh`), then set `hosts.app: mlxlab.example.org` and
`make deploy`. mlxlab's iOS app and TUI keep their URL; their devices sign in again once (the device tokens of
the old hub are in its own `devices.json`, which has the same format: copying it into ai-connector's `/data`
before the switch should keep them — untested).

## Connecting workers

In the console: **Workers → Connect a worker**, name the machine, copy the one paste for macOS/Linux (Terminal) or
Windows (PowerShell) and run it there. It downloads the worker program of this build from `/tools/mlxlab/<platform>`
with the worker's own token and starts it; the console shows "waiting → connected". Revoke a worker in the same
list. Workers on Linux need glibc (not Alpine); see the README for what each platform runs.

## Backups

Everything worth keeping is the JSON in `/data` (a few KB; the files are listed in "What is in `/data`"). The image is distroless (no shell, `cat` or `tar`, so
`kubectl cp` and `kubectl exec … cat` do not work): read the volume on the node instead. The local-path PV names its
directory on the Pi:

```sh
export KUBECONFIG=<gitops checkout>/kubeconfig
P=$(kubectl get pv "$(kubectl -n ai-connector get pvc ai-connector-data -o jsonpath='{.spec.volumeName}')" \
  -o jsonpath='{.spec.local.path}{.spec.hostPath.path}')
ssh user@pi.lan "sudo tar -C '$P' -czf - ." > ai-connector-data-$(date +%F).tgz
```

Restore: scale to 0 (`kubectl -n ai-connector scale deploy/ai-connector --replicas=0`), untar into the same path as
root, `chown -R 65532:65532`, scale back to 1. An uninstall keeps the PVC (`helm.sh/resource-policy: keep`); delete
it with `kubectl -n ai-connector delete pvc ai-connector-data` to really start over.

## Building and CI

- `make image` — the image for linux/arm64 into the local Docker (`pharndt/ai-connector-server:dev`).
- `make image-multiarch` — linux/arm64 + linux/amd64 with buildx, as CI builds it; nothing is pushed
  (`MULTIARCH_OUTPUT=--push` pushes).
- `make chart-lint` — `helm lint --strict` and `helm template` with `ci/lint-values.yaml`.
- `make check` — gofmt, `go vet` for the four platforms, `go test ./...`, the chart.
- `.github/workflows/ci.yml` — on every push and PR: the Go checks, `pnpm install --frozen-lockfile && pnpm test &&
  pnpm build` in `web/` and `console/`, the chart.
- `.github/workflows/server-image.yml` — on a `server-v*` tag: the multi-arch image to Docker Hub
  (`:<version>` and `:latest`) and a GitHub release.
