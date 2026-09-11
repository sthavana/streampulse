.PHONY: build run test vet fmt check docker compose-up compose-down clean

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

docker:
	docker build -t streampulse:dev .

# The demo stack: prober + Prometheus + Grafana against public test streams.
compose-up:
	docker compose -f deploy/docker-compose.yml up --build -d

compose-down:
	docker compose -f deploy/docker-compose.yml down

clean:
	rm -rf bin
