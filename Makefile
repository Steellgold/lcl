.PHONY: build test check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/lcl .

test:
	go test -race ./...

check:
	test -z "$$(gofmt -l *.go)"
	go vet ./...
	go test -race ./...
