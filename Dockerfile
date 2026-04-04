# ── Stage 1: Build Go binary ─────────────────────────────────────────
FROM golang:1.23-alpine AS go-build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /meridian ./cmd/meridian

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

COPY --from=go-build /meridian /app/meridian
COPY --from=ui-build /ui/dist /app/dashboard/dist

# Default config
COPY meridian.yaml /app/meridian.yaml

EXPOSE 8080 9090 7946

VOLUME ["/app/data"]

ENTRYPOINT ["/app/meridian"]
CMD ["serve"]
