FROM golang:1.26.6-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /dashboard ./cmd/dashboard

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /dashboard /dashboard
EXPOSE 9000
ENTRYPOINT ["/dashboard"]
