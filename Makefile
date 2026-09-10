.PHONY: build run test vet fmt clean

build:
	go build -o bin/prober ./cmd/prober

run: build
	./bin/prober -config config.json

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin
