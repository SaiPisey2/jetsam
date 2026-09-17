FROM golang:1.27-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init loadgen && CGO_ENABLED=0 go build -o /loadgen .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /loadgen /loadgen
EXPOSE 8081
ENTRYPOINT ["/loadgen"]
