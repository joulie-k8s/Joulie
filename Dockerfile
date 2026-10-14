# Base images are named by tag and pinned by digest, which is what the build
# pulls; change both together. The golang tag is the go.mod toolchain.
FROM golang:1.26.9@sha256:37cfcca63222ec499bec5e8ad7aad52dbf7529db36d5812b8eae2f0621aa92d6 AS builder
WORKDIR /src
ARG COMPONENT=agent

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY api ./api
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/joulie ./cmd/${COMPONENT}

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=builder /out/joulie /joulie
ENTRYPOINT ["/joulie"]
