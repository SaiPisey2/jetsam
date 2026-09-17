.PHONY: build test demo-up demo-down demo-logs demo-test demo-vendor

build:
	CGO_ENABLED=0 go build -o jetsam ./cmd/jetsam

test:
	go test ./...

# Build and start are separate steps on purpose: `up -d --build` can hang
# with containers stuck in Created, which shows up as an indefinite wait
# rather than a build error.
demo-up:
	docker-compose -f demo/docker-compose.yml build
	docker-compose -f demo/docker-compose.yml up -d
	docker-compose -f demo/docker-compose.yml ps
	bash demo/grafana/token.sh
	@echo "prometheus http://localhost:9090"
	@echo "grafana    http://localhost:3000"

demo-down:
	docker-compose -f demo/docker-compose.yml down -v
	rm -f demo/querylog/queries.log demo/grafana/.token

demo-logs:
	docker-compose -f demo/docker-compose.yml logs -f

demo-vendor:
	bash demo/vendor.sh

# -count=1 is mandatory rather than decorative: without it the test cache
# can return a PASS for a target that never touched the running stack.
demo-test:
	go test -tags=integration ./demo/... -count=1 -v
