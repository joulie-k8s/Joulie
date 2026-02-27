FROM golang:1.23 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY simulator ./simulator
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/joulie-simulator ./simulator/cmd/simulator

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/joulie-simulator /joulie-simulator
USER 65532:65532
ENTRYPOINT ["/joulie-simulator"]
