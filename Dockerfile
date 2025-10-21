FROM golang:1.25.0-alpine AS builder

WORKDIR /app

ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off
ENV GOPRIVATE=*
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

RUN apk add --no-cache git ca-certificates tzdata openssl && \
    git config --global http.sslverify false && \
    git config --global http.postBuffer 1048576000

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod \
    GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=off \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
    -ldflags="-w -s" \
    -o /app/bin/service \
    ./cmd/pilot

RUN mkdir -p /app/config && \
    if [ -d "./app/${SERVICE_PATH}/config" ]; then \
        cp -r ./config/config.yaml /app/config/config.yaml 2>/dev/null || true; \
    fi

FROM alpine:latest

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=builder /app/bin/service /service

COPY --from=builder /app/config /config/

EXPOSE 8080

CMD ["/service"]