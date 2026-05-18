# Build the manager binary
FROM golang:1.23 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Cache dependencies first so the deps layer is reused unless go.mod/sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# CGO off for static-link, GOOS/GOARCH from buildx, strip debug info.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -a -o manager cmd/main.go

# Use distroless for runtime. Smaller, fewer CVEs, no shell. If you need
# to exec into the container for debugging, swap to gcr.io/distroless/static:debug.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
