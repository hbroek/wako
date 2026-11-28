# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
ENV CGO_ENABLED=0
RUN for c in wakod wako; do \
      go build -trimpath -ldflags "-s -w -X github.com/hbroek/wako/internal/version.Version=${VERSION} -X github.com/hbroek/wako/internal/version.Commit=${COMMIT} -X github.com/hbroek/wako/internal/version.Date=${DATE}" \
        -o /out/$c ./cmd/$c || exit 1; done \
 && mkdir -p /out/data/spaces /out/tmp && chmod 1777 /out/tmp && chown -R 65532:65532 /out/data && chmod 700 /out/data

# Nothing but the two programs and the CA bundle (the AI service is reached over
# HTTPS, and without trusted roots every call fails with "unknown authority").
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/wakod /out/wako /usr/local/bin/
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data
COPY --from=build --chown=65532:65532 --chmod=1777 /out/tmp /tmp
USER 65532:65532
ENV WAKO_DATA_DIR=/data WAKO_ADDR=0.0.0.0:8888 WAKO_LOG_FORMAT=json
VOLUME /data
EXPOSE 8888
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/wakod", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/wakod"]
