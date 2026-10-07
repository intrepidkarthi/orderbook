# The exchange in a box (docs/EXCHANGE-IN-A-BOX.md): one image, four binaries.
# compose.yaml picks the binary per service with `command:`.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/obgw ./cmd/obquote ./cmd/obsoak ./cmd/obdash

FROM alpine:3.20
# busybox wget is the health check; nothing else is added.
RUN adduser -D -u 10001 venue && mkdir /data && chown venue /data
COPY --from=build /out/ /usr/local/bin/
USER venue
WORKDIR /data
