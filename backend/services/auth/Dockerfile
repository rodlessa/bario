# Build the requested Go service and ship a small static binary image.
FROM golang:1.24-alpine AS build

WORKDIR /src

COPY go.work ./
COPY pkg ./pkg
COPY services ./services

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/chirp-auth ./services/auth

FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 chirp \
    && adduser -S -u 10001 -G chirp chirp

COPY --from=build /out/chirp-auth /usr/local/bin/chirp-auth

USER 10001:10001

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/chirp-auth"]
