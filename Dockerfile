# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/peregrine .
# bbolt needs a directory it can create a file in and write to. A fresh named
# volume inherits the ownership and mode of the image path it is mounted over,
# the distroless base has no /data, and it has no shell to mkdir one at
# runtime, so the directory has to be built here and copied in owned by the
# nonroot uid. Without this, the first run on a new host fails inside
# bbolt.Open with permission denied on a root-owned volume root.
#
# Numeric 65532 rather than the name nonroot: --chown resolves names against
# the *target* image's /etc/passwd, and the numeric form cannot be wrong.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/peregrine /peregrine
COPY --from=builder --chown=65532:65532 /out/data /data
USER nonroot:nonroot
ENTRYPOINT ["/peregrine"]
