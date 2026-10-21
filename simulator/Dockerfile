# Base images are named by tag and pinned by the digest of their multi-arch
# index, which is what the build pulls; change both together. The golang tag
# is the go.mod toolchain.
#
# The builder runs on the build host's platform and cross-compiles for the
# target one (docker buildx build --platform linux/amd64,linux/arm64), so no
# stage needs emulation: the final stage only copies the static binaries.
FROM --platform=$BUILDPLATFORM golang:1.26.9@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS builder
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY pkg ./pkg
COPY simulator ./simulator
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/joulie-simulator ./simulator/cmd/simulator
# fakesmi stands in for nvidia-smi, rocm-smi and amd-smi on emulated nodes. In
# emulate mode the simulator copies it into the tree root (SIM_FAKESMI_SOURCE),
# and the agent pods run it from there, so it must be static.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/fakesmi ./simulator/cmd/fakesmi

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=builder /out/joulie-simulator /joulie-simulator
COPY --from=builder /out/fakesmi /usr/local/lib/joulie/fakesmi
USER 65532:65532
ENTRYPOINT ["/joulie-simulator"]
