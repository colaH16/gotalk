# 빌드 스테이지
FROM golang:1.23-alpine AS builder
WORKDIR /app

# [중요] go.mod와 go.sum을 먼저 복사해야 라이브러리를 받을 수 있음
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o cotalk-server main.go

# 실행 스테이지
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/cotalk-server .
EXPOSE 8080
CMD ["./cotalk-server"]
