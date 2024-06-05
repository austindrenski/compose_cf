# syntax = docker/dockerfile:1.7-labs
FROM --platform="${BUILDPLATFORM}" golang:1.22.4-alpine3.19 AS build
WORKDIR /build/

COPY --link --parents go.mod .
COPY --link --parents go.sum .

RUN --mount=type=cache,target=/root/.cache/go-build/ \
    --mount=type=cache,target=/go/pkg/ \
    go mod download

COPY --link --parents cmd/** .

ARG TARGETARCH
ARG TARGETOS
RUN CGO_ENABLED=0 GOARCH="${TARGETARCH}" GOOS="${TARGETOS}" go build -ldflags="-s -w" -o /app/compose_cf cmd/compose_cf.go

FROM --platform="${TARGETPLATFORM}" scratch

COPY --link --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --link --from=build /app/compose_cf /usr/local/bin/

CMD []
ENTRYPOINT ["compose_cf"]
USER 1001:1001
