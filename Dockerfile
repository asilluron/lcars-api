# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
# Also in the Makefile (NODE).
ARG NODE_VERSION=24.22.1

# The admin page (admin/), built into internal/adminui/dist to be embedded.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-slim AS admin
WORKDIR /src/admin
COPY admin/package.json admin/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY admin/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
COPY --from=admin /src/internal/adminui/dist/ internal/adminui/dist/
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/asilluron/lcars-api/internal/server.version=${VERSION}" \
      -o /out/lcars-api ./cmd/lcars-api

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/lcars-api /lcars-api
# The server reads lcars.toml from its working directory if present, so a
# config file can be mounted at /config/lcars.toml.
WORKDIR /config
# 4222: the embedded NATS server (the API). 8080: HTTP (health, docs, JSON).
EXPOSE 4222 8080
USER nonroot:nonroot
ENTRYPOINT ["/lcars-api"]
