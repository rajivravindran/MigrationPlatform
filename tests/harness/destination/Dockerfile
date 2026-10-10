# Test-only destination fixture for the integrity harness. Never deploy.
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY tests/harness/destination/go.mod ./
COPY tests/harness/destination/*.go ./
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /out/destination .

FROM alpine:3.20
RUN adduser -D -u 10001 app && mkdir -p /data && chown app /data
USER app
COPY --from=builder /out/destination /app/destination
EXPOSE 8099
ENTRYPOINT ["/app/destination"]
