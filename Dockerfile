# 빌드 스테이지
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o cotalk-server main.go

# 실행 스테이지
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/cotalk-server .
EXPOSE 8080
CMD ["./cotalk-server"]
