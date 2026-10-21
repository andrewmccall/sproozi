FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY hack/verify/mcp-provider/main.go ./main.go
RUN CGO_ENABLED=0 go build -o /provider ./main.go
FROM scratch
COPY --from=builder /provider /provider
USER 65532:65532
ENTRYPOINT ["/provider"]
