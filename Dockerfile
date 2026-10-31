FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/dost ./cmd/dost

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
LABEL org.opencontainers.image.title="dost" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.source="https://github.com/sarpaydemir/dost" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/dost /usr/local/bin/dost
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 CMD ["/usr/local/bin/dost", "health"]
ENTRYPOINT ["/usr/local/bin/dost"]
CMD ["run"]
