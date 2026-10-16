# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /qrelay .

# Static binary, no shell, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /qrelay /qrelay
ENV QRELAY_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/qrelay", "-healthcheck"]
ENTRYPOINT ["/qrelay"]
