FROM golang:1.24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/cpa-manager ./cmd/cpa-manager

# Create /data with the nonroot ownership in the build stage so the final COPY
# can carry it over. A named volume mounted at /data is initialised from the
# image's directory, and if that directory is root:root 0755 the unprivileged
# process cannot write its state file there.
RUN mkdir -p /data && chown 65532:65532 /data && chmod 0755 /data

FROM gcr.io/distroless/static-debian12:nonroot
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/Newoahil/CPA-Manager" \
      org.opencontainers.image.licenses="LGPL-3.0-only" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/cpa-manager /usr/local/bin/cpa-manager
# Retain the base image's own notices, and accompany our static executable with
# project/dependency terms plus the license of the Go runtime used to build it.
COPY LICENSE COPYING COPYING.LESSER THIRD_PARTY_NOTICES.md /usr/share/licenses/cpa-manager/
COPY licenses/third-party/ /usr/share/licenses/cpa-manager/third-party/
COPY --from=build /usr/local/go/LICENSE /usr/share/licenses/cpa-manager/third-party/go-LICENSE
# --chown makes /data (and thus the freshly-initialised named volume) writable
# by the nonroot runtime user (uid/gid 65532). Numeric ids are used so the copy
# does not depend on the destination image's user database.
COPY --from=build --chown=65532:65532 /data /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/cpa-manager"]
