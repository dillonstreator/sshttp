FROM golang:1.21 as builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . ./

RUN CGO_ENABLED=0 go build -o server cmd/server/main.go

FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/server .

CMD ["./server"]
