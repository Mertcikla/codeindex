# syntax=docker/dockerfile:1
FROM golang:1.26.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go test -c -o /out/ ./tools ./lsp ./indexer

FROM node:24-bookworm
COPY --from=build /out/ /tests/
COPY testdata/typescript-compat/run.sh /tests/run.sh
ENTRYPOINT ["sh", "/tests/run.sh"]
