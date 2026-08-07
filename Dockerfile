# --platform pins the build stage to the runner's own architecture. Without it,
# building the linux/arm64 image would run the Go compiler under emulation and
# turn a one minute build into ten; with it, Go cross-compiles natively, which
# it is good at.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# The SQLite driver is pure Go, so the image needs no cgo toolchain and the
# binary runs on any machine. It is also what makes the cross-compile above
# free: there is no C toolchain to obtain for the target architecture.
ENV CGO_ENABLED=0

# Supplied by BuildKit from the platform being built.
ARG TARGETOS
ARG TARGETARCH
ENV GOOS=$TARGETOS GOARCH=$TARGETARCH

# The tag, when the image is built from one. Empty for a local `docker build`,
# which is the honest answer.
ARG RELEASE=""

# .git is deliberately outside the build context, which keeps it at kilobytes;
# the cost is that `sonda -version` reports no revision from inside the image
# and says so rather than implying one. The tag above is the part worth having.
RUN go build -trimpath -ldflags="-s -w -X main.release=$RELEASE" -o /out/sonda ./cmd/sonda
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
