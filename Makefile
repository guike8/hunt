VERSION ?= dev

.PHONY: build test check bench release clean

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/hunt .

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem

release:
	python3 scripts/release.py --version $(VERSION)

clean:
	go clean
	rm -rf bin dist
