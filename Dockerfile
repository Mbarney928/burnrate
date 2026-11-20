# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
# Pure-Go SQLite driver, so the binary is fully static and needs no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/burnrate ./cmd/burnrate \
 && mkdir -p /out/data

# ---- runtime --------------------------------------------------------------
# Distroless: no shell or package manager; runs as the unprivileged "nonroot" user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/burnrate /usr/local/bin/burnrate
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
ENV BURNRATE_ADDR=0.0.0.0:8080 \
    BURNRATE_DB_PATH=/data/burnrate.db
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/burnrate"]
CMD ["serve"]
