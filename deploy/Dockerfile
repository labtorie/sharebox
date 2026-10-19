# Coordination server image. Build from the repository root:
#   docker build -f deploy/Dockerfile -t sharebox-server .
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
VOLUME /data
ENV SHAREBOX_DATA_DIR=/data
EXPOSE 443 80 3478/udp 3478/tcp 49160-49999/udp
ENTRYPOINT ["/sharebox-server"]
