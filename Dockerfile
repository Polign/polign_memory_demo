FROM golang:1.25.1-alpine3.22@sha256:b6ed3fd0452c0e9bcdef5597f29cc1418f61672e9d3a2f55bf02e7222c014abd AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go predicates.json ./
COPY memkit ./memkit
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/polign-memory-demo .

FROM alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

RUN mkdir -p /var/lib/polign-memory && chmod 0770 /var/lib/polign-memory
COPY --from=build /out/polign-memory-demo /usr/local/bin/polign-memory-demo

USER 65532:0
ENTRYPOINT ["/usr/local/bin/polign-memory-demo"]
