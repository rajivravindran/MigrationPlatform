FROM golang:1.22-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY apps/orchestrator-go/go.mod apps/orchestrator-go/go.sum* ./
RUN go mod download
COPY apps/orchestrator-go ./
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /out/worker ./cmd/worker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app
USER app
WORKDIR /app
COPY --from=builder /out/worker /app/worker
EXPOSE 9464
ENTRYPOINT ["/app/worker"]
