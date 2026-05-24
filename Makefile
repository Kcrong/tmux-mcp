.PHONY: build test fmt vet tidy clean

BIN := tmux-mcp

build:
	go build -trimpath -o $(BIN) ./cmd/tmux-mcp

test:
	go test ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -f $(BIN)
