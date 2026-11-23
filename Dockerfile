# Build the server binary, then run it on a minimal image.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hobbs ./cmd/hobbs

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 hobbs
COPY --from=build /out/hobbs /usr/local/bin/hobbs
USER hobbs
EXPOSE 8080
# Configuration is by flag (set the start command on the host); secrets come
# from the environment: HOBBS_NMS_CLIENT_ID, HOBBS_NMS_CLIENT_SECRET.
ENTRYPOINT ["/usr/local/bin/hobbs"]
CMD ["-http", ":8080"]
