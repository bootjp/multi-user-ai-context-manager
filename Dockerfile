FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /multi-user-ai-context-manager ./cmd/multi-user-ai-context-manager

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /multi-user-ai-context-manager /multi-user-ai-context-manager
COPY LICENSE /LICENSE
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/multi-user-ai-context-manager"]
CMD ["serve"]
