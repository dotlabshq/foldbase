# foldbase — a single static binary, no CGO (modernc.org/sqlite).
#
# Build context is this directory: the Go module is the whole image, so nothing
# else in the repo needs to reach the daemon. See .dockerignore beside this file.
#
# The runtime is distroless/static, not alpine or debian-slim: with CGO off the
# binary needs no libc at all, so a base that ships one — plus a shell and a
# package manager — would only add size and attack surface.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Cross-compile from the builder's own architecture rather than emulating the
# target. A pure-Go build needs nothing from the target platform, so a multi-arch
# `buildx` run costs one native compile per arch instead of a QEMU one.
ARG TARGETOS TARGETARCH
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /foldbase .

# :nonroot runs as uid 65532 — the plain tag is root, and nothing here needs it.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /foldbase /foldbase
ENV PORT=3001
EXPOSE 3001
ENTRYPOINT ["/foldbase"]
