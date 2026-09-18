.PHONY: build test demo-up demo-down demo-logs demo-test demo-vendor

# docker-compose (v1, hyphenated) is gone from GitHub-hosted ubuntu-latest
# runners; only the `docker compose` v2 plugin ships there now. Override
# on hosts that still need the standalone binary: make COMPOSE=docker-compose demo-up
COMPOSE ?= docker compose

build:
	CGO_ENABLED=0 go build -o jetsam ./cmd/jetsam

test:
	go test ./...

# Build and start are separate steps on purpose: `up -d --build` can hang
# with containers stuck in Created, which shows up as an indefinite wait
# rather than a build error.
#
# fixture/querylog is bind-mounted into the prometheus container so the host
# (and later, jetsam itself) can read the query log back out. It is
# gitignored, so a fresh checkout does not have it, and Docker would create
# it itself on first use -- owned by root, since dockerd runs as root --
# while prom/prometheus runs as the unprivileged `nobody` user and cannot
# write into a root-owned directory. Create it ourselves, world-writable,
# before the stack starts. 777 is fine here only because this is a
# gitignored scratch directory in a local test fixture with nothing
# sensitive in it; it is not a pattern to reuse anywhere that matters.
demo-up:
	mkdir -p fixture/querylog
	chmod 777 fixture/querylog
	$(COMPOSE) -f fixture/docker-compose.yml build
	$(COMPOSE) -f fixture/docker-compose.yml up -d
	$(COMPOSE) -f fixture/docker-compose.yml ps
	bash fixture/grafana/token.sh
	bash fixture/ready.sh
	bash fixture/query.sh
	@echo "prometheus http://localhost:9090"
	@echo "grafana    http://localhost:3000"

demo-down:
	$(COMPOSE) -f fixture/docker-compose.yml down -v
	rm -f fixture/querylog/queries.log fixture/grafana/.token

demo-logs:
	$(COMPOSE) -f fixture/docker-compose.yml logs -f

demo-vendor:
	bash fixture/vendor.sh

# -count=1 is mandatory rather than decorative: without it the test cache
# can return a PASS for a target that never touched the running stack.
demo-test:
	go test -tags=integration ./fixture/... -count=1 -v
