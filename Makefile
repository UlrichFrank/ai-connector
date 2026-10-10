.PHONY: build web web-dev console console-dev test vet worker serve server server-linux

# local.mk (gitignored, see local.mk.example): this machine's settings, e.g. the deploy target (PI, GITOPS, AIC_URL)
-include local.mk

# the build's version (mlxlab version, the hub's log): the commit, -dirty with local changes
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"

build: web console
	go build -trimpath $(LDFLAGS) -o bin/ai-connector ./cmd/ai-connector

# the React web app (web/), built into internal/webui/dist and embedded into the binary
web:
	cd web && pnpm install --frozen-lockfile --silent && pnpm build

# the console of ai-connector (console/), built into internal/consoleui/dist and embedded, served under /console/
console:
	cd console && pnpm install --frozen-lockfile --silent && pnpm build

# the server (web UI, sign-in, proxy to the workers) for this machine, and for the Raspberry Pi (no cgo)
server: web console
	go build -trimpath $(LDFLAGS) -o bin/ai-connector-server ./cmd/ai-connector-server

server-linux: web console
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath $(LDFLAGS) -o bin/ai-connector-server-linux-arm64 ./cmd/ai-connector-server

# the web app with hot reload on http://localhost:5173, against a running mlxlab (MLXLAB_URL, default :8484)
web-dev:
	cd web && pnpm dev

# the console with hot reload on http://localhost:5173/console/, against a running server (MLXLAB_URL)
console-dev:
	cd console && pnpm dev

test:
	go test ./...

vet:
	go vet ./...

# what CI checks (.github/workflows/ci.yml): gofmt, vet for every platform a binary ships for, the tests
VET_PLATFORMS ?= darwin/arm64 linux/amd64 linux/arm64 windows/amd64
.PHONY: fmt-check vet-all check
fmt-check:
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet-all:
	@for p in $(VET_PLATFORMS); do echo "go vet $$p"; GOOS=$${p%/*} GOARCH=$${p#*/} go vet ./... || exit 1; done

check: fmt-check vet-all test chart-lint

# this machine as a worker of the server (server URL and token: flags, env or ~/.config/mlxlab/worker.json)
worker: build
	bin/ai-connector worker

# standalone on http://127.0.0.1:8484, no hub (development)
serve: build
	bin/ai-connector serve

# ---------- the server image, the Helm chart, the deploy to the Raspberry Pi (docs/deploy.md) ----------
.PHONY: image image-multiarch base-images chart-lint deploy deploy-image deploy-status deploy-guard deploy-env

DOCKER      ?= docker --context rancher-desktop
IMAGE       ?= pharndt/ai-connector-server
IMAGE_TAG   ?= dev
PLATFORMS   ?= linux/arm64,linux/amd64
CHART       := deploy/helm/ai-connector
DISTROLESS  := gcr.io/distroless/static-debian12:nonroot
BASE_IMAGES := node:24-alpine golang:1.27-alpine $(DISTROLESS)

# the server's image (Dockerfile) for the Raspberry Pi: linux/arm64, into the local Docker as $(IMAGE):$(IMAGE_TAG).
# Releases are built and pushed by .github/workflows/server-image.yml on tags server-v*.
image: base-images
	$(DOCKER) build --pull=false --platform linux/arm64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) -f Dockerfile .

# both platforms of the release (what CI builds), with buildx; nothing is pushed or loaded unless
# MULTIARCH_OUTPUT says so (e.g. MULTIARCH_OUTPUT=--push, or --output type=oci,dest=bin/server.oci.tar).
# Needs a builder that reaches the registries for the amd64 base image (CI does; Rancher Desktop's VM may not).
MULTIARCH_OUTPUT ?= --output type=cacheonly
image-multiarch:
	$(DOCKER) buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGE_TAG) -f Dockerfile $(MULTIARCH_OUTPUT) .

# Rancher Desktop's VM may not reach the registries (gcr.io, at times Docker Hub); Apple's container CLI on the Mac
# does: a missing arm64 base image is pulled there and loaded into Docker
base-images:
	@for img in $(BASE_IMAGES); do \
		$(DOCKER) image inspect $$img >/dev/null 2>&1 || $(DOCKER) pull --platform linux/arm64 $$img || \
		(container image pull --platform linux/arm64 $$img && \
		 container image save --platform linux/arm64 -o /tmp/ai-connector-base.tar $$img && \
		 $(DOCKER) load -i /tmp/ai-connector-base.tar && rm -f /tmp/ai-connector-base.tar) || exit 1; \
	done

# the Helm chart (deploy/helm/ai-connector), with ci/lint-values.yaml for the required password
chart-lint:
	helm lint --strict $(CHART) -f $(CHART)/ci/lint-values.yaml
	helm template ai-connector $(CHART) -n ai-connector -f $(CHART)/ci/lint-values.yaml >/dev/null
	@# deploy's rollout check finds the Deployment by the chart's fullname rules
	@test "$$($(call deployment_name,ai-connector,))" = ai-connector
	@test "$$($(call deployment_name,aic-test,))" = aic-test-ai-connector
	@test "$$($(call deployment_name,aic-test,--set fullnameOverride=custom))" = custom
	@test "$$($(call deployment_name,aic-test,--set nameOverride=server))" = aic-test-server

# ---- deploy to the Raspberry Pi (k0s), without a registry ----
# Run by hand only (docs/deploy.md). The image is built here for linux/arm64, streamed over ssh into the Pi's
# containerd, and the in-repo chart is upgraded with that tag (pullPolicy Never) and the values from the gitops
# repo. Its own release and namespace (ai-connector): it never touches the mlxlab release (namespace mlxlab).
# The gitops kubeconfig is used explicitly, whatever kubectl's current context is.
# PI and GITOPS have no default: set them in local.mk (see local.mk.example) or on the command line.
PI            ?=
GITOPS        ?=
AIC_RELEASE   ?= ai-connector
AIC_NAMESPACE ?= ai-connector
# values.yaml (hosts, models, storage …) and secrets.yaml (sops; helm-secrets) of this release in the gitops repo
AIC_VALUES    ?= $(GITOPS)/cluster/charts/apps/ai-connector/values
AIC_SECRETS   ?= $(AIC_VALUES)/secrets.yaml
# the console host name (hosts.console), checked after the deploy
AIC_URL       ?= https://ai.example.org
KUBE          ?= KUBECONFIG=$(GITOPS)/kubeconfig
# the commit, plus -dirty-<time> with local changes (a new tag each time, so the pod restarts)
DEPLOY_TAG    := $(shell git rev-parse --short HEAD 2>/dev/null)$(shell git diff --quiet HEAD -- . 2>/dev/null || echo -dirty-$$(date +%s))
# helm-secrets decrypts secrets.yaml when there is one; without it the values must name an existingSecret
HELM_UPGRADE   = $(if $(wildcard $(AIC_SECRETS)),helm secrets upgrade,helm upgrade)
SECRETS_VALUES = $(if $(wildcard $(AIC_SECRETS)),-f $(AIC_SECRETS))

# deployment_name prints the name of the release's Deployment as the chart makes it (its fullname rules:
# fullnameOverride, nameOverride, a release name that contains the chart's): rendered by helm itself, with the
# release's values (a placeholder password: the name does not depend on the secrets)
deployment_name = helm template $(1) $(CHART) $(2) --set-string secrets.MLXLAB_PASSWORD=name-only -s templates/deployment.yaml \
	| awk '/^kind: Deployment/{d=1} d && /^  name:/{print $$2; exit}'

deploy: deploy-guard deploy-image
	$(KUBE) $(HELM_UPGRADE) --install -n $(AIC_NAMESPACE) --create-namespace $(AIC_RELEASE) $(CHART) \
		-f $(AIC_VALUES)/values.yaml $(SECRETS_VALUES) \
		--set image.repository=$(IMAGE) --set image.tag=$(DEPLOY_TAG) --set image.pullPolicy=Never \
		--wait --timeout 3m
	$(KUBE) kubectl -n $(AIC_NAMESPACE) rollout status deploy/$$($(call deployment_name,$(AIC_RELEASE),-f $(AIC_VALUES)/values.yaml)) --timeout=120s
	@# older ai-connector images on the Pi go (the running pod uses the new one now); mlxlab's are not matched
	ssh $(PI) 'sudo k0s ctr -n k8s.io images ls -q | grep "$(IMAGE):" | grep -v ":$(DEPLOY_TAG)$$" | xargs -r sudo k0s ctr -n k8s.io images rm' >/dev/null || true
	@curl -fsS --retry 15 --retry-all-errors --retry-delay 2 $(AIC_URL)/healthz && echo && echo "deployed $(IMAGE):$(DEPLOY_TAG) to $(AIC_URL)"

deploy-image: deploy-guard base-images
	$(DOCKER) build --pull=false --platform linux/arm64 --build-arg VERSION=$(DEPLOY_TAG) -t $(IMAGE):$(DEPLOY_TAG) -f Dockerfile .
	$(DOCKER) save $(IMAGE):$(DEPLOY_TAG) | ssh $(PI) 'sudo k0s ctr -n k8s.io images import --platform linux/arm64 -'

deploy-status: deploy-env
	$(KUBE) kubectl -n $(AIC_NAMESPACE) get pods,ingress,pvc
	@curl -fsS $(AIC_URL)/healthz; echo

# the deploy's target must be named: PI (ssh) and GITOPS (kubeconfig, values), in local.mk or on the command line
deploy-env:
	@test -n "$(PI)" || { echo "PI is not set: the ssh target of the k0s node (user@host), in local.mk (see local.mk.example) or make PI=..." >&2; exit 1; }
	@test -n "$(GITOPS)" || { echo "GITOPS is not set: the gitops checkout (kubeconfig, values), in local.mk (see local.mk.example) or make GITOPS=..." >&2; exit 1; }

# the running mlxlab hub has the release and namespace "mlxlab" and the image pharndt/mlxlab-hub: never these
deploy-guard: deploy-env
	@case "$(AIC_RELEASE) $(AIC_NAMESPACE) $(IMAGE)" in *mlxlab*) \
		echo "refusing: release/namespace/image ($(AIC_RELEASE) $(AIC_NAMESPACE) $(IMAGE)) names mlxlab's hub" >&2; exit 1;; esac
	@test -f $(AIC_VALUES)/values.yaml || { echo "no $(AIC_VALUES)/values.yaml (see docs/deploy.md)" >&2; exit 1; }

.PHONY: sandbox-files-image sandbox-files-test sandbox-files-try

# the files sandbox of the chat (sandbox/files, internal/sandbox/files): the image is built on first use anyway;
# this builds it ahead (tag = hash of the build context, so the worker finds it)
sandbox-files-image:
	go run ./internal/sandbox/files/cmd/sandbox-files-try -build-only

# integration tests against Apple `container` (network, size, time and memory limits)
sandbox-files-test:
	go test -tags sandbox -count=1 -v -timeout 30m ./internal/sandbox/files/

# demo: a one-page PDF report with a chart and a PDF from Markdown, into sandbox-files-out/
sandbox-files-try:
	go run ./internal/sandbox/files/cmd/sandbox-files-try -out sandbox-files-out

# ---------- the web sandbox (internal/sandbox/web, sandbox/web) ----------
# The pf anchor on the Mac: the second wall behind the in-VM lockdown (see sandbox/web/pf.conf). Needs sudo;
# pf rules do not survive a reboot, run it again after one. With the sandbox running, the interface rules
# (IPv6, spoofed sources) are loaded too.
SANDBOX_INT_NET ?= 192.168.240.0/24
SANDBOX_EXT_NET ?= 192.168.241.0/24
SANDBOX_INT_GW  ?= 192.168.240.1
SANDBOX_EXT_GW  ?= 192.168.241.1
SANDBOX_PF_MACROS = -D int_net=$(SANDBOX_INT_NET) -D ext_net=$(SANDBOX_EXT_NET) -D int_gw=$(SANDBOX_INT_GW) -D ext_gw=$(SANDBOX_EXT_GW)
# the interface (vmnet bridge) that has the given address
sandbox_bridge = $$(ifconfig | awk -v ip="$(1)" '/^[a-z]/ {i=$$1; sub(":$$","",i)} $$1=="inet" && $$2==ip {print i}')

.PHONY: sandbox-firewall sandbox-firewall-off sandbox-firewall-status sandbox-check

sandbox-firewall:
	sudo pfctl -a com.apple/mlxlab-web $(SANDBOX_PF_MACROS) -f sandbox/web/pf.conf
	@int_if=$(call sandbox_bridge,$(SANDBOX_INT_GW)); ext_if=$(call sandbox_bridge,$(SANDBOX_EXT_GW)); \
	if [ -n "$$int_if" ] && [ -n "$$ext_if" ]; then \
		echo "bridges: $$int_if (isolated network), $$ext_if (proxy network)"; \
		echo "sudo pfctl -a com.apple/mlxlab-web-if ... -f sandbox/web/pf-if.conf"; \
		sudo pfctl -a com.apple/mlxlab-web-if $(SANDBOX_PF_MACROS) -D int_if=$$int_if -D ext_if=$$ext_if -f sandbox/web/pf-if.conf; \
	else \
		echo "the sandbox is not running: interface rules (IPv6, spoofed sources) not loaded; run again while it is up"; \
	fi
	sudo pfctl -E

sandbox-firewall-off:
	sudo pfctl -a com.apple/mlxlab-web -F all
	sudo pfctl -a com.apple/mlxlab-web-if -F all

sandbox-firewall-status:
	sudo pfctl -s info | head -1
	sudo pfctl -a com.apple/mlxlab-web -s rules
	sudo pfctl -a com.apple/mlxlab-web-if -s rules

# the isolation check against the real sandbox (starts it, probes from inside every VM, stops it)
sandbox-check:
	go test -tags sandbox -v -count=1 -timeout 30m -run TestIsolation ./internal/sandbox/web

# ---------- the local integration: server, this Mac as worker, a local vboard, the connector (docs/local.md) ----------
.PHONY: local-up local-down local-status local-logs local-open
local-up:
	deploy/local/local.sh up
local-down:
	deploy/local/local.sh down
local-status:
	deploy/local/local.sh status
local-logs:
	deploy/local/local.sh logs
local-open:
	deploy/local/local.sh open
