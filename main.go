package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
)

var nc *nats.Conn
var db *sql.DB
var hostname string

type Message struct {
	ID         int    `json:"id"`
	Content    string `json:"content"`
	SenderPod  string `json:"sender_pod"`
	SenderNick string `json:"sender_nick"`
	SenderColor string `json:"sender_color"` // 색상 코드 추가
	Time       string `json:"time"`
}

type User struct {
	Nickname  string `json:"nickname"`
	ColorCode string `json:"color_code"`
}

func main() {
	hostname, _ = os.Hostname()
	initDB()
	initNATS()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/stream", streamHandler)
	http.HandleFunc("/send", sendHandler)
	http.HandleFunc("/history", historyHandler) // 과거 내역 조회
	http.HandleFunc("/login", loginHandler)     // 닉네임/색상 조회

	port := "8080"
	log.Printf("🥤 CoTalk Server started on %s (Pod: %s)", port, hostname)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func initDB() {
	// (기존 DB 연결 로직 유지...)
	dbHost := os.Getenv("DB_HOST")
	dbUser := os.Getenv("DB_USER")
	dbPwd := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	if dbName == "" { dbName = "cotalk" }

	// 1. Temp DB 접속 및 DB 생성 (기존과 동일)
	psqlInfo := fmt.Sprintf("host=%s user=%s password=%s dbname=postgres sslmode=disable", dbHost, dbUser, dbPwd)
	tempDB, err := sql.Open("postgres", psqlInfo)
	if err != nil { log.Fatal(err) }
	var exists bool
	tempDB.QueryRow("SELECT EXISTS(SELECT datname FROM pg_catalog.pg_database WHERE datname = $1)", dbName).Scan(&exists)
	if !exists { tempDB.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName)) }
	tempDB.Close()

	// 2. 실제 DB 접속
	connStr := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, dbUser, dbPwd, dbName)
	db, err = sql.Open("postgres", connStr)
	if err != nil { log.Fatal(err) }
	if err := db.Ping(); err != nil { log.Fatal(err) }

	// 3. 테이블 생성 (users 테이블 추가됨!)
	// messages 테이블에 sender_color 추가
	queries := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id SERIAL PRIMARY KEY,
			content TEXT,
			sender_pod TEXT,
			sender_nick TEXT,
			sender_color TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS users (
			nickname TEXT PRIMARY KEY,
			color_code TEXT
		);`,
	}
	
	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			log.Printf("Schema Warning (Table might exist): %v", err)
		}
	}
}

func initNATS() {
	// (기존 NATS 연결 로직 유지)
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" { natsURL = nats.DefaultURL }
	var err error
	nc, err = nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil { log.Fatal(err) }
}

// 닉네임 체크 및 색상 반환
func loginHandler(w http.ResponseWriter, r *http.Request) {
	nick := r.URL.Query().Get("nick")
	var color string
	// DB에서 닉네임으로 색상 조회
	err := db.QueryRow("SELECT color_code FROM users WHERE nickname = $1", nick).Scan(&color)
	
	resp := User{Nickname: nick}
	if err == nil {
		resp.ColorCode = color // 저장된 색상이 있음
	} else {
		resp.ColorCode = ""    // 저장된 색상 없음 (클라이언트가 물어봐야 함)
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// 과거 내역 페이징 조회
func historyHandler(w http.ResponseWriter, r *http.Request) {
	beforeIDStr := r.URL.Query().Get("before_id")
	limit := 30 // 기본 30개

	query := "SELECT id, content, sender_pod, sender_nick, COALESCE(sender_color, '#ffffff'), to_char(created_at, 'HH24:MI:SS') FROM messages"
	var rows *sql.Rows
	var err error

	// before_id가 있으면 그보다 이전 글만 조회 (더 불러오기)
	if beforeIDStr != "" {
		beforeID, _ := strconv.Atoi(beforeIDStr)
		query += " WHERE id < $1 ORDER BY id DESC LIMIT $2"
		rows, err = db.Query(query, beforeID, limit)
	} else {
		// 없으면 최신 글 조회
		query += " ORDER BY id DESC LIMIT $1"
		rows, err = db.Query(query, limit)
	}

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var history []Message
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.Content, &m.SenderPod, &m.SenderNick, &m.SenderColor, &m.Time)
		history = append(history, m)
	}
	
	// JSON 응답
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	content := r.FormValue("msg")
	nickname := r.FormValue("nick")
	color := r.FormValue("color")

	if content == "" || nickname == "" { return }
	if color == "" { color = "#ffffff" }

	// 1. Users 테이블 업데이트 (닉네임-색상 저장/갱신)
	// PostgreSQL UPSERT 구문
	_, err := db.Exec(`
		INSERT INTO users (nickname, color_code) VALUES ($1, $2)
		ON CONFLICT (nickname) DO UPDATE SET color_code = $2`, 
		nickname, color)
	if err != nil { log.Println("User Update Error:", err) }

	// 2. Messages 테이블 저장
	var id int
	err = db.QueryRow(
		"INSERT INTO messages (content, sender_pod, sender_nick, sender_color) VALUES ($1, $2, $3, $4) RETURNING id",
		content, hostname, nickname, color,
	).Scan(&id)
	
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// 3. NATS 전송
	msg := Message{
		ID:         id,
		Content:    content,
		SenderPod:  hostname,
		SenderNick: nickname,
		SenderColor: color,
		Time:       time.Now().Format("15:04:05"),
	}
	data, _ := json.Marshal(msg)
	nc.Publish("chat.global", data)
	w.WriteHeader(http.StatusOK)
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// *이제 접속 시 과거 내역을 여기서 주지 않습니다.* (history API 사용)

	sub, err := nc.SubscribeSync("chat.global")
	if err != nil { return }
	defer sub.Unsubscribe()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		default:
			m, err := sub.NextMsg(1 * time.Second)
			if err == nats.ErrTimeout {
				fmt.Fprintf(w, ":keepalive\n\n")
				w.(http.Flusher).Flush()
				continue
			}
			if err != nil { return }
			fmt.Fprintf(w, "data: %s\n\n", string(m.Data))
			w.(http.Flusher).Flush()
		}
	}
}