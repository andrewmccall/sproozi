FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY hack/verify/orchestration-kind/provider/*.go ./hack/verify/orchestration-kind/provider/
RUN CGO_ENABLED=0 go build -o /provider ./hack/verify/orchestration-kind/provider
FROM scratch
COPY --from=build /provider /provider
USER 65532:65532
ENTRYPOINT ["/provider"]
