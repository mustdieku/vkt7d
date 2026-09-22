BINDIR ?= bin

.PHONY: build test clean

build:
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/vkt7d ./cmd/vkt7d
	go build -o $(BINDIR)/vkt7check ./cmd/vkt7check
	go build -o $(BINDIR)/vkt7dbtest ./cmd/vkt7dbtest

test:
	go test ./...

clean:
	rm -rf $(BINDIR)
