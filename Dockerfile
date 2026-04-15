# ── Stage 1: Build Go binaries ───────────────────────────────────────
FROM golang:1.25-alpine AS go-build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /meridian       ./cmd/meridian
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway        ./cmd/gateway
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ingestor       ./cmd/ingestor
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /storage-svc    ./cmd/storage
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /querier        ./cmd/querier
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /compactor      ./cmd/compactor

# ── Stage 2: Build dashboard ────────────────────────────────────────
FROM node:20-alpine AS ui-build
WORKDIR /ui
COPY dashboard/package.json dashboard/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY dashboard/ .
RUN npx vite build

# ── Stage 3: Runtime ─────────────────────────────────────────────────
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

COPY --from=go-build /meridian       /app/meridian
COPY --from=go-build /gateway        /app/gateway
COPY --from=go-build /ingestor       /app/ingestor
COPY --from=go-build /storage-svc    /app/storage-svc
COPY --from=go-build /querier        /app/querier
COPY --from=go-build /compactor      /app/compactor
COPY --from=ui-build /ui/dist        /app/dashboard/dist

# Default config
COPY meridian.yaml /app/meridian.yaml

EXPOSE 8080 9090 7946

VOLUME ["/app/data"]

ENTRYPOINT ["/app/meridian"]
CMD ["serve"]
