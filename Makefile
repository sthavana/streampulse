.PHONY: build run test vet fmt check clean

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

clean:
	rm -rf bin
