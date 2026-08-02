# Dragoman as a container: a Go builder, then a scratch image holding the static
# binary and the CA bundle and nothing else. Nothing outside this repository is
# read, so a git URL is a complete build context and a machine holding neither a
# clone nor a Go toolchain can still build the image.

FROM golang:1.25-alpine AS build

# The certificate bundle is installed in the builder because the final stage has
# no package manager. Without it every HTTPS call to a provider fails at the
# handshake, which reads as a provider outage rather than a missing file.
RUN apk add --no-cache ca-certificates

WORKDIR /src

# The manifests are their own layer so that editing a source file does not
# refetch the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off is what makes the binary static, which is what allows a scratch final
# stage. -s -w drop the symbol table and DWARF, -trimpath keeps build paths out
# of the binary; together they are most of the difference between an image a
# stranger builds in seconds and one they abandon.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /dragoman ./cmd/dragoman

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /dragoman /dragoman

# Nothing else is copied on purpose. No provider key, no service table, no
# config file: credentials arrive as environment variables, and a table that is
# not the embedded default is mounted read-only at run time, so the image is the
# same bytes for every deployment.

# Numeric, because scratch carries no /etc/passwd for a name to resolve against.
# The process reads its configuration and writes its log, and needs no identity
# beyond one that owns nothing.
USER 65532:65532

EXPOSE 8080

# The address is set here rather than left to the flag because the flag's
# default is loopback — correct for a process on a laptop, unreachable for a
# sibling container, and the symptom is a connection refused that reads as a
# wrong hostname. Naming it in CMD makes `docker run` alone correct and spares
# every compose file from remembering the flag, while leaving both the flag and
# the other subcommands reachable by overriding the command.
ENTRYPOINT ["/dragoman"]
CMD ["serve", "--addr", "0.0.0.0:8080"]

# The binary checks itself, because a scratch image holds one file and no shell,
# no curl and no wget — any other command named here would be one the image
# cannot execute, leaving a container permanently unhealthy rather than one that
# is not yet listening. It asks only whether this process is serving; reaching a
# provider is deliberately not part of the answer, or an instance would be
# restarted for someone else's outage.
HEALTHCHECK --interval=5s --timeout=3s --start-period=2s --retries=5 \
    CMD ["/dragoman", "healthcheck", "--addr", "127.0.0.1:8080"]
