# Multi-stage Go build for boaky. The SQLite driver (modernc.org/sqlite) is
# pure Go with FTS5 enabled, so CGO_ENABLED=0 keeps the image tiny and
# architecture-portable (works on Apple Silicon and x86 hosts alike).
FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/boaky ./cmd/boaky

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/boaky /usr/local/bin/boaky
USER 65532:65532
WORKDIR /data

ENV BOAKY_HTTP_ADDR=0.0.0.0:8000 \
    BOAKY_BOOKS_DIR=/books \
    BOAKY_DB_PATH=/data/index/boaky.db \
    BOAKY_META_DIR=/data/metadata \
    BOAKY_LOG_LEVEL=info

# books: read-only source; index+metadata: persistent bind mounts.
# No published ports: Hermes reaches this over an internal network.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["boaky", "health"]

ENTRYPOINT ["/usr/local/bin/boaky"]
CMD ["serve-http"]
