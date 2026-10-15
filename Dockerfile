FROM golang:1.24-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o teleagent2api .

FROM alpine:3.21
RUN apk --no-cache add ca-certificates wget \
    && adduser -D -u 10001 appuser
WORKDIR /app
COPY --from=builder /build/teleagent2api .
# Credentials arrive via environment variables owned by the runtime; keep
# the image root-owned and read-only for the app user.
RUN chown -R root:root /app && chmod -R 555 /app
USER appuser
EXPOSE 10000
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD wget -qO- http://localhost:10000/health || exit 1
CMD ["./teleagent2api"]
