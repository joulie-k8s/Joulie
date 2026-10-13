FROM golang:1.23 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY pkg ./pkg
COPY simulator ./simulator
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/joulie-simulator ./simulator/cmd/simulator
# fakesmi stands in for nvidia-smi, rocm-smi and amd-smi on emulated nodes. In
# emulate mode the simulator copies it into the tree root (SIM_FAKESMI_SOURCE),
# and the agent pods run it from there, so it must be static.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/fakesmi ./simulator/cmd/fakesmi

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/joulie-simulator /joulie-simulator
COPY --from=builder /out/fakesmi /usr/local/lib/joulie/fakesmi
USER 65532:65532
ENTRYPOINT ["/joulie-simulator"]
