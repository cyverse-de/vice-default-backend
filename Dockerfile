FROM golang:1.21 as build-root

WORKDIR /build

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

RUN go build ./...


# Second stage
FROM golang:1.16

COPY --from=build-root /build/vice-default-backend /bin/vice-default-backend

ENTRYPOINT ["vice-default-backend"]
CMD ["--help"]

EXPOSE 60000
