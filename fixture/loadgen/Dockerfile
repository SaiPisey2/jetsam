# Pinned by digest, not just tag: a floating base image means this
# fixture can change what it measures without anyone touching the repo.
FROM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
WORKDIR /src
COPY main.go .
RUN go mod init loadgen && CGO_ENABLED=0 go build -o /loadgen .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /loadgen /loadgen
EXPOSE 8081
ENTRYPOINT ["/loadgen"]
