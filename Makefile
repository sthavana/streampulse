.PHONY: build run test vet fmt check check-linux docker docker-full compose-up compose-down clean

build:
	go build -o bin/prober ./cmd/prober

run: build
	./bin/prober -config config.json

test:
	go test ./...

vet:
	go vet ./...

# check runs what CI runs, so a red pipeline is not the first you hear of it.
# Keep in step with .github/workflows/ci.yml.
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...

fmt:
	gofmt -w .

# CI runs on Linux, where /bin/sh is dash and process groups behave
# differently. Two bugs have reached CI that macOS could not reproduce, so this
# runs the same suite in the same environment before pushing.
check-linux:
	docker run --rm -v "$$PWD":/src -w /src golang:1.22 go test -race ./...

docker:
	docker build -t streampulse:dev .

# The inspection image: same binary, on a base that has ffprobe.
docker-full:
	docker build -f Dockerfile.full -t streampulse:dev-full .

# The demo stack: prober + Prometheus + Grafana against public test streams.
compose-up:
	docker compose -f deploy/docker-compose.yml up --build -d

compose-down:
	docker compose -f deploy/docker-compose.yml down

clean:
	rm -rf bin
