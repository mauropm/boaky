.PHONY: build test race fmt vet run index serve docker up down build-docker bench clean

BIN := bin/boaky

build:
	go build -trimpath -o $(BIN) ./cmd/boaky

test:
	go test ./...

race:
	go test -race ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet test race

index: build
	./$(BIN) index

status: build
	./$(BIN) status

serve: build
	BOAKY_HTTP_ADDR=127.0.0.1:8000 ./$(BIN) serve-http

docker:
	docker compose build --pull

up:
	docker compose up -d

down:
	docker compose down   # never -v: named/bind data must survive

recreate:
	docker compose up -d --force-recreate --build

index-docker:
	docker compose run --rm boaky index

bench: build
	./scripts/benchmark.sh

clean:
	rm -rf bin /tmp/boaky-bench
