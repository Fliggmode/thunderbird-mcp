# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/thunderbird-mcp .

FROM alpine:3.20
# CA certificates are required for IMAP/SMTP over TLS.
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/thunderbird-mcp /usr/local/bin/thunderbird-mcp
# The server speaks MCP over stdio and reads a Thunderbird profile (mount it read-only).
ENTRYPOINT ["/usr/local/bin/thunderbird-mcp"]
