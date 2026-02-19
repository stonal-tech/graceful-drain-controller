FROM golang:1.23 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o manager .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
