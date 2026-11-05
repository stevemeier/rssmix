CGO_ENABLED ?= 0
export CGO_ENABLED

VERSION := $(shell git rev-parse --short HEAD)

GOLDFLAGS += -X main.version=$(VERSION)
GOFLAGS = -ldflags "$(GOLDFLAGS)"

all: rssmix-api rssmix-compiler rssmix-fetcher rssmix-publisher

clean:
	rm -f rssmix-api rssmix-compiler rssmix-fetcher rssmix-publisher

rssmix-api: FORCE
	go build -trimpath -ldflags="-s -w" -o rssmix-api $(GOFLAGS) api/main.go

rssmix-compiler: FORCE
	go build -trimpath -ldflags="-s -w" -o rssmix-compiler $(GOFLAGS) ./compiler

rssmix-fetcher: FORCE
	go build -trimpath -ldflags="-s -w" -o rssmix-fetcher $(GOFLAGS) ./fetcher

rssmix-publisher: FORCE
	go build -trimpath -ldflags="-s -w" -o rssmix-publisher $(GOFLAGS) publisher/main.go

FORCE: ;
