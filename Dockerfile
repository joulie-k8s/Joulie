FROM golang:1.22 AS builder
WORKDIR /src
ARG COMPONENT=agent

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/joulie ./cmd/${COMPONENT}

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/joulie /joulie
ENTRYPOINT ["/joulie"]
