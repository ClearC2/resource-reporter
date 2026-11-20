GO = docker run --rm -v "$(CURDIR)":/src -v resource-reporter-gocache:/root/.cache/go-build -w /src golang:1.27

.PHONY: test vet build

test:
	$(GO) go test -race ./...

vet:
	$(GO) go vet ./...

build:
	$(GO) env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=2.0.0" -o resource-reporter.linux-amd64 .
	shasum -a 256 resource-reporter.linux-amd64 > resource-reporter.linux-amd64.sha256
