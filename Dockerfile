FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# The SQLite driver is pure Go, so the image needs no cgo toolchain and the
# binary runs on any machine.
ENV CGO_ENABLED=0

# .git is deliberately outside the build context, which keeps it at kilobytes;
# the cost is that `sonda -version` reports no revision from inside the image
# and says so rather than implying one.
RUN go build -trimpath -ldflags="-s -w" -o /out/sonda ./cmd/sonda
RUN go build -trimpath -ldflags="-s -w" -o /out/echo ./examples/echo
RUN go build -trimpath -ldflags="-s -w" -o /out/grpcdemo ./examples/grpcdemo
RUN go build -trimpath -ldflags="-s -w" -o /out/sonda-tui ./cmd/sonda-tui

FROM alpine:3.21

RUN adduser -D -u 10001 sonda && mkdir -p /data && chown sonda:sonda /data
COPY --from=build /out/sonda /usr/local/bin/sonda
COPY --from=build /out/echo /usr/local/bin/echo
COPY --from=build /out/grpcdemo /usr/local/bin/grpcdemo
COPY --from=build /out/sonda-tui /usr/local/bin/sonda-tui

USER sonda
VOLUME /data
WORKDIR /data

ENTRYPOINT ["sonda"]
CMD ["-config", "/etc/sonda/sonda.yaml"]
