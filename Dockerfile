# Fly does not take a bare binary, so the single-binary build is wrapped in a
# minimal image. Templates, static assets and migrations are all embedded via
# go:embed, so the runtime stage carries nothing but the binary and CA certs.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a code change does not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off: both the libSQL client and the SQLite driver are pure Go, which
# keeps this a static binary and the image tiny.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/doot ./cmd/doot

FROM alpine:3.21

# TLS roots are needed to reach Turso, the model API, Daytona and GitHub.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 doot

COPY --from=build /out/doot /usr/local/bin/doot

USER doot
EXPOSE 8080

ENV PORT=8080

# Migrations run inside the binary on startup, so deploying is the only action
# required. There is no separate release command.
ENTRYPOINT ["/usr/local/bin/doot"]
CMD ["serve"]
