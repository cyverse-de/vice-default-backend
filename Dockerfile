FROM golang:1.27 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0

RUN go build -o vice-default-backend .


FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /build/vice-default-backend /bin/vice-default-backend

ENTRYPOINT ["vice-default-backend"]

EXPOSE 60000
