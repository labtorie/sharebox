# Sharebox coordination server for Docker hosting platforms (PaaS) that
# expose a single HTTP port behind their own HTTPS proxy.
#
# Required setting (environment variable or run argument):
#   SHAREBOX_PUBLIC_URL=https://<your-app-domain>
#
# Limitations of this mode: platforms like this don't forward UDP, so the
# TURN relay can't run here. Peer-to-peer WebRTC works for most guests via
# public STUN servers; guests on very restrictive networks (some mobile
# carriers, corporate firewalls) won't be able to connect. For full coverage
# use a VPS (deploy/ubuntu) or set SHAREBOX_TURN_URLS/SECRET to an external
# coturn relay.
FROM node:22-alpine AS web
WORKDIR /src/apps/guest-web
COPY apps/guest-web/package.json apps/guest-web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY apps/guest-web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/guestweb/dist ./internal/guestweb/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /sharebox-server ./cmd/sharebox-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sharebox-server /sharebox-server
ENV SHAREBOX_LISTEN=:8080 \
    SHAREBOX_PLAIN_HTTP=1 \
    SHAREBOX_TRUST_PROXY=1 \
    SHAREBOX_DATA_DIR=/data \
    SHAREBOX_STUN_URLS=stun:stun.l.google.com:19302,stun:stun.cloudflare.com:3478
EXPOSE 8080
ENTRYPOINT ["/sharebox-server"]
