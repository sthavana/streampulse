# Build a static binary. CGO off so nothing links against the build image's
# libc, which is what lets the result run on scratch.
#
# The Go version tracks the go directive in go.mod. CI additionally tests
# against stable, so a newer toolchain is known to work if you bump this.
FROM golang:1.22-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /prober ./cmd/prober

# scratch, not alpine: the prober has no dependencies, shells out to nothing,
# and embeds its own timezone database (time/tzdata in main.go), so there is
# nothing else it needs at runtime. The image is the binary and a CA bundle.
# Nothing else in it can carry a CVE.
FROM scratch

# HTTPS to origins and CDNs needs the root certificates. Without these every
# fetch fails with "certificate signed by unknown authority".
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /prober /prober

# nobody. There is no /etc/passwd on scratch, so this has to be numeric.
USER 65534:65534

EXPOSE 9090

# No HEALTHCHECK: scratch has no shell and no curl to run one with. The binary
# serves /healthz, which is what an orchestrator's HTTP probe should point at.
ENTRYPOINT ["/prober"]
CMD ["-config", "/etc/streampulse/config.json"]
