# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gamesdb ./cmd/gamesdb

FROM alpine:3.21
# /data must exist and belong to the runtime user before VOLUME: a new named
# volume takes the ownership of the image's directory, and the server creates
# uploads/ and covers/ inside it.
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 gamesdb && mkdir -p /data && chown gamesdb:gamesdb /data
COPY --from=build /gamesdb /usr/local/bin/gamesdb
USER gamesdb
ENV DATA_DIR=/data LISTEN_ADDR=:8080 INTERNAL_LISTEN_ADDR=127.0.0.1:8081
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["gamesdb"]
CMD ["serve"]
