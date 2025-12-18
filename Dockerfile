FROM golang:1.25 as builder

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
FROM gcr.io/distroless/static-debian13:nonroot

WORKDIR /vice-default-backend
COPY --from=builder /build/static ./static

COPY --from=builder /build/vice-default-backend /bin/vice-default-backend

ENTRYPOINT ["vice-default-backend"]
CMD ["--help"]

EXPOSE 60000
