# memoryweb pulls in sqlite-vec and go-sqlite3, both of which are cgo. A plain
# CGO_ENABLED=0 build fails, and a scratch runtime image has no libc to link
# against — so this is a glibc builder and a glibc runtime, deliberately.
#
# The builder must be >= the go directive in memoryweb's go.mod (1.24.13) or the
# build spends time downloading a toolchain on every deploy.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first so edits to our own code don't re-download the world.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

# Per-tenant SQLite lives here; mount a volume at this path or every deploy
# throws away all customer data.
RUN mkdir -p /data
VOLUME /data
ENV DATA_DIR=/data PORT=8080

COPY --from=build /out/server /usr/local/bin/server

EXPOSE 8080
# "serve" is explicit because running the binary with no arguments now means
# quickstart — the friendly local setup for someone who double-clicked it. A
# server should say what it wants rather than rely on a default.
ENTRYPOINT ["/usr/local/bin/server", "serve"]
