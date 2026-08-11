# One image for all six toy services; the compose entrypoint picks which binary
# runs. They are a few hundred lines each, so building them separately would
# cost more in layers than it saves.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.21

COPY --from=build /out/ /usr/local/bin/
USER nobody
