.PHONY: build web web-dev console console-dev test vet worker serve server server-linux

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

# this machine as a worker of the server (server URL and token: flags, env or ~/.config/mlxlab/worker.json)
worker: build
	bin/ai-connector worker

# standalone on http://127.0.0.1:8484, no hub (development)
serve: build
	bin/ai-connector serve

.PHONY: image

# the server's container image (Dockerfile) for the Raspberry Pi, loaded into the local Docker as
# pharndt/ai-connector-server:dev; releases are built and pushed by .github/workflows/server-image.yml on tags server-v*
image:
	docker buildx build --platform linux/arm64 -t pharndt/ai-connector-server:dev -f Dockerfile --load .

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
