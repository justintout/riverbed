# Riverbed is pure Go, so the final image needs no libc and no shell.
FROM golang:1.26-alpine AS build

ARG VERSION=dev
ENV CGO_ENABLED=0

WORKDIR /src

# Dependencies first, so edits to the source do not refetch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -tags osusergo,netgo \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/riverbed ./cmd/riverbed

# distroless/static carries CA certificates and time zone data, which the
# agent and embedding HTTP clients need, and nothing else.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/riverbed /usr/local/bin/riverbed

# The database, and the cache for a downloaded embedding model, live here.
# Declaring it as a volume keeps the data out of the container layer.
VOLUME /var/lib/riverbed
ENV RIVERBED_DB=/var/lib/riverbed/riverbed.db \
    RIVERBED_CONFIG=/etc/riverbed/riverbed.toml \
    XDG_CACHE_HOME=/var/lib/riverbed/cache \
    GO_POTION_HOME=/var/lib/riverbed/models

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/riverbed"]
CMD ["serve"]
