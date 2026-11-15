FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agent-context ./cmd/agent-context

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /agent-context /agent-context
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/agent-context"]
CMD ["serve"]
