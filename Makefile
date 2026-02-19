.PHONY: build test lint fmt clean

build:
	go build -o graceful-drain-controller .

test:
	go test -v -race -count=1 ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

clean:
	rm -f graceful-drain-controller
