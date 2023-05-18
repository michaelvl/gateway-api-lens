FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY gateway-api-lens /gateway-api-lens
USER 65532:65532

ENTRYPOINT ["/gateway-api-lens"]
