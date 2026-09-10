# syntax=docker/dockerfile:1

# Build stage. Cross-compilation is done by Go rather than by emulation, so a
# multi-arch build runs at native speed on a single builder.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-w -s \
        -X github.com/hauke-cloud/sandman/internal/version.version=${VERSION} \
        -X github.com/hauke-cloud/sandman/internal/version.commit=${COMMIT} \
        -X github.com/hauke-cloud/sandman/internal/version.date=${DATE}" \
      -o /out/sandman \
      ./cmd/sandman

# Runtime stage. distroless/static carries CA certificates and a nonroot user
# and nothing else -- no shell, no package manager, nothing to exploit. The
# CustomResourceDefinition sandman installs is embedded in the binary, so
# nothing has to be copied alongside it.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/sandman /usr/local/bin/sandman

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/sandman"]
