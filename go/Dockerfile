# foldbase (Go) — a single static binary, no CGO (modernc.org/sqlite).
# Build context is this directory. The whole engine lives in the binary, so
# there is no npm publish step in the release chain (unlike the TS image).
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /foldbase .

FROM gcr.io/distroless/static-debian12
COPY --from=build /foldbase /foldbase
ENV PORT=3001
EXPOSE 3001
ENTRYPOINT ["/foldbase"]
