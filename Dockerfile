FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mailbox-tunnel .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /mailbox-tunnel /mailbox-tunnel
ENV STATUS_LISTEN=:7860
EXPOSE 7860
ENTRYPOINT ["/mailbox-tunnel", "-mode", "node"]
