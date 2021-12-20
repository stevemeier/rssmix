VERSION := $(shell git rev-parse --short HEAD)

GOLDFLAGS += -X main.version=$(VERSION)
GOFLAGS = -ldflags "$(GOLDFLAGS)"

all: rssmix-api rssmix-compiler rssmix-fetcher rssmix-publisher

clean:
	rm -f rssmix-api rssmix-compiler rssmix-fetcher rssmix-publisher

rssmix-api:
	go build -o rssmix-api $(GOFLAGS) api/main.go

rssmix-compiler:
	go build -o rssmix-compiler $(GOFLAGS) compiler/main.go

rssmix-fetcher:
	go build -o rssmix-fetcher $(GOFLAGS) fetcher/main.go

rssmix-publisher:
	go build -o rssmix-publisher $(GOFLAGS) publisher/main.go
