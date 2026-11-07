CGO_ENABLED ?= 0
export CGO_ENABLED

VERSION := $(shell git rev-parse --short HEAD)

GOLDFLAGS += -s -w -X main.version=$(VERSION)
BUILD_FLAGS = -trimpath -ldflags "$(GOLDFLAGS)"

all: rssmix-api rssmix-compiler rssmix-fetcher rssmix-publisher

clean:
	rm -f rssmix-api rssmix-compiler rssmix-fetcher rssmix-publisher

rssmix-api: FORCE
	go build -o rssmix-api $(BUILD_FLAGS) ./api

rssmix-compiler: FORCE
	go build -o rssmix-compiler $(BUILD_FLAGS) ./compiler

rssmix-fetcher: FORCE
	go build -o rssmix-fetcher $(BUILD_FLAGS) ./fetcher

rssmix-publisher: FORCE
	go build -o rssmix-publisher $(BUILD_FLAGS) publisher/main.go

FORCE: ;
