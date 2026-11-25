# libpgquery 需要 CGO；运行镜像仅携带编译后的二进制和 glibc，不带 Go/GCC。
FROM golang:1.26-bookworm AS build
WORKDIR /source
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /pam ./cmd/pam
FROM debian:bookworm-slim
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /pam /pam
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/pam"]
