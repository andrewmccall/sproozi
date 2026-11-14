FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY hack/verify/mcp-provider/main.go ./main.go
RUN CGO_ENABLED=0 go build -o /provider ./main.go
FROM scratch
COPY --from=builder /provider /provider
USER 65532:65532
ENTRYPOINT ["/provider"]
