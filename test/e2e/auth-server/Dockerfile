ARG GO_VERSION=1.26
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder

WORKDIR /workspace
COPY test/e2e/auth-server/main.go ./main.go
RUN CGO_ENABLED=0 go build -o /auth-server ./main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /auth-server /auth-server
USER 65532:65532
ENTRYPOINT ["/auth-server"]
