# Build stage: cgo + libvips headers for bimg
FROM golang:1.26-alpine AS build
RUN apk add --no-cache build-base vips-dev pkgconfig
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /out/server ./cmd/server

# Runtime stage: libvips shared libs only, non-root user
FROM alpine:3.21
RUN apk add --no-cache vips ca-certificates \
    && adduser -S -u 10001 -H app \
    && mkdir -p /data/images \
    && chown app /data/images
COPY --from=build /out/server /usr/local/bin/server
USER app
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=5 \
  CMD ["server", "healthcheck"]
ENTRYPOINT ["server"]
