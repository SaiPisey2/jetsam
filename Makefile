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
demo-up:
	$(COMPOSE) -f demo/docker-compose.yml build
	$(COMPOSE) -f demo/docker-compose.yml up -d
	$(COMPOSE) -f demo/docker-compose.yml ps
	bash demo/grafana/token.sh
	bash demo/ready.sh
	bash demo/query.sh
	@echo "prometheus http://localhost:9090"
	@echo "grafana    http://localhost:3000"

demo-down:
	$(COMPOSE) -f demo/docker-compose.yml down -v
	rm -f demo/querylog/queries.log demo/grafana/.token

demo-logs:
	$(COMPOSE) -f demo/docker-compose.yml logs -f

demo-vendor:
	bash demo/vendor.sh

# -count=1 is mandatory rather than decorative: without it the test cache
# can return a PASS for a target that never touched the running stack.
demo-test:
	go test -tags=integration ./demo/... -count=1 -v
