FROM golang:1.22 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/joulie-agent ./cmd/agent

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/joulie-agent /joulie-agent
USER 65532:65532
ENTRYPOINT ["/joulie-agent"]
