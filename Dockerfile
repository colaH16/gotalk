# ==========================================
# 1. Frontend Builder (Node.js)
# ==========================================
FROM node:24 AS frontend-builder
WORKDIR /app/frontend

# 패키지 설정 복사 & 설치
COPY frontend/package.json frontend/tailwind.config.js ./
RUN npm install

# 소스 복사 & CSS 빌드
# (이제 static 폴더 없이 frontend 폴더에 바로 파일들이 있음)
COPY frontend/input.css ./
COPY frontend/index.html ./
RUN npm run build
# -> 결과물: /app/frontend/output.css 생성됨

# ==========================================
# 2. Backend Builder (Go)
# ==========================================

# 빌드 스테이지
FROM golang:1.25 AS backend-builder
WORKDIR /app/backend

# [중요] go.mod와 go.sum을 먼저 복사해야 라이브러리를 받을 수 있음
COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -o cotalk-server main.go

# ==========================================
# 3. Final Runner (실행 이미지)
# ==========================================
FROM alpine:latest
WORKDIR /root/

# 1) 백엔드 실행 파일 가져오기
COPY --from=backend-builder /app/backend/cotalk-server .

# 2) 정적 파일(HTML, CSS) 조립하기
# 서버는 ./static 폴더를 바라보게 되어 있으니, 여기서 폴더를 만들어 줍니다.
RUN mkdir -p ./static

# 프론트엔드 빌드 결과물(index.html, output.css)을 static 폴더로 쏙!
COPY --from=frontend-builder /app/frontend/index.html ./static/
COPY --from=frontend-builder /app/frontend/output.css ./static/

EXPOSE 8080
CMD ["./cotalk-server"]
