.PHONY: proto build build-ubuntu run clean

proto:
	cd proto && buf generate

build:
	mkdir -p bin
	go build -ldflags "-w -s" -o bin/xdp-mcp .

build-ubuntu:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-w -s" -o bin/xdp-mcp-linux-amd64 .

run: build
	./bin/xdp-mcp

clean:
	rm -rf bin/
