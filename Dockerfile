# Registry ownership marker — must match the server name in server.json:
# io.github.lisycotana/superbtmr
FROM golang:1.25-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/superbtmr .

FROM alpine
LABEL io.modelcontextprotocol.server.name="io.github.lisycotana/superbtmr"

# Dedicated non-root user with a fixed uid/gid so bind-mount owners map
# predictably; superbtmr resolves its data dir to ~/.superbtmr by default, so the
# whole state (sessions, SSH configs, message history) lives in the HOME volume.
RUN addgroup -S -g 1000 superbtmr \
    && adduser -S -u 1000 -G superbtmr -h /home/superbtmr superbtmr

COPY --from=build /out/superbtmr /usr/local/bin/superbtmr

USER superbtmr
ENV HOME=/home/superbtmr
WORKDIR /home/superbtmr
VOLUME /home/superbtmr

# No EXPOSE/ENTRYPOINT: the image only carries the binary, and the deployment
# decides how to run it. Start superbtmr explicitly and pass the bind address it
# should listen on, e.g.
#   docker run -d -p 18765:18765 -e SUPERBTMR_AUTH_TOKEN=... ghcr.io/lisycotana/superbtmr:latest superbtmr --host 0.0.0.0 --port 18765
