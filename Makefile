BINDIR ?= bin

.PHONY: build test clean

build:
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/vkt7d ./cmd/vkt7d

test:
	go test ./...

clean:
	rm -rf $(BINDIR)
