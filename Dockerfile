FROM dhi/golang:1.25 as build-root

WORKDIR /build

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

RUN go build -o vice-default-backend .


# Second stage
FROM dhi/debian-base:trixie

WORKDIR /vice-default-backend
COPY --from=build-root /build/static ./static

COPY --from=build-root /build/vice-default-backend /bin/vice-default-backend

ENTRYPOINT ["vice-default-backend"]
CMD ["--help"]

EXPOSE 60000
