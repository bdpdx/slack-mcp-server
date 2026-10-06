# Base images are pinned by digest; refresh with
#   docker buildx imagetools inspect golang:1.25
#   docker buildx imagetools inspect alpine:3.22
FROM golang:1.25@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80 AS build

ENV CGO_ENABLED=0
ENV GOTOOLCHAIN=local
ENV GOCACHE=/go/pkg/mod

WORKDIR /app

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . /app

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -mod=readonly -ldflags="-s -w" -o /go/bin/mcp-server ./cmd/slack-mcp-server

FROM build AS dev

RUN --mount=type=cache,target=/go/pkg/mod \
    go install github.com/go-delve/delve/cmd/dlv@v1.25.0 && cp /go/bin/dlv /dlv

WORKDIR /app/mcp-server

EXPOSE 13080

ENTRYPOINT ["mcp-server"]
CMD ["--transport", "sse"]

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS production

RUN apk add --no-cache ca-certificates \
  && addgroup -S -g 10001 app \
  && adduser -S -D -u 10001 -G app -h /home/app app \
  && install -d -m 0700 -o app -g app /home/app/.cache /home/app/.cache/slack-mcp-server

COPY --from=build /go/bin/mcp-server /usr/local/bin/mcp-server

USER 10001:10001
WORKDIR /home/app

EXPOSE 13080

ENTRYPOINT ["mcp-server"]
CMD ["--transport", "sse"]
