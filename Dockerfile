FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 go build -o gofin ./cmd/gofin

FROM alpine:latest
RUN apk add --no-cache ca-certificates tini
COPY --from=builder /build/gofin /usr/local/bin/
COPY --from=builder /build/gofin.example.json /etc/gofin/gofin.json.example

# Create working directory with proper permissions
RUN mkdir -p /app/data /app/cache /app/logs /app/config && \
    chmod 755 /app /app/data /app/cache /app/logs /app/config

WORKDIR /app

EXPOSE 8096
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["gofin", "serve", "--log-level", "info"]
