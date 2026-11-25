# seatimed for container hosts (Orchard). A static binary, no cgo.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/seatimed ./cmd/seatimed

FROM gcr.io/distroless/static-debian13
COPY --from=build /out/seatimed /seatimed
# Set SEATIME_DB to a postgres:// URL and SEATIME_SECRET to a random key
# (openssl rand -hex 32). The rest is in docs/SERVER.md.
ENV SEATIME_ADDR=:8080 SEATIME_TRUST_PROXY=1
EXPOSE 8080
# Probes: GET /healthz (503 while the database does not answer, or once
# shutdown has begun). On SIGTERM seatimed drains requests for up to 15 s,
# so keep the grace period at 20 s or more. Logs are JSON lines on stdout.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/seatimed"]
