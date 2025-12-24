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
	ID          int    `json:"id"`
	Content     string `json:"content"`
	SenderPod   string `json:"sender_pod"`
	SenderNick  string `json:"sender_nick"`
	SenderColor string `json:"sender_color"` // DB엔 없지만 JSON 응답용으로 존재
	Time        string `json:"time"`
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
	http.HandleFunc("/history", historyHandler)
	http.HandleFunc("/login", loginHandler)

	port := "8080"
	log.Printf("🥤 CoTalk Server started on %s (Pod: %s)", port, hostname)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func initDB() {
	dbHost := os.Getenv("DB_HOST")
	dbUser := os.Getenv("DB_USER")
	dbPwd := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	if dbName == "" { dbName = "cotalk" }

	// 1. Temp DB 접속 및 생성
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

	// 3. 테이블 생성
	// [변경] messages 테이블에서 sender_color 제거! (닉네임으로 조인할거니까)
	queries := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id SERIAL PRIMARY KEY,
			content TEXT,
			sender_pod TEXT,
			sender_nick TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS users (
			nickname TEXT PRIMARY KEY,
			color_code TEXT
		);`,
	}
	
	for _, query := range queries {
		if _, err := db.Exec(query); err != nil {
			log.Printf("Schema Warning: %v", err)
		}
	}
}

func initNATS() {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" { natsURL = nats.DefaultURL }
	var err error
	nc, err = nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil { log.Fatal(err) }
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	nick := r.URL.Query().Get("nick")
	var color string
	err := db.QueryRow("SELECT color_code FROM users WHERE nickname = $1", nick).Scan(&color)
	
	resp := User{Nickname: nick}
	if err == nil {
		resp.ColorCode = color
	} else {
		resp.ColorCode = ""
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// [핵심 변경] JOIN 쿼리 사용
func historyHandler(w http.ResponseWriter, r *http.Request) {
	beforeIDStr := r.URL.Query().Get("before_id")
	limit := 30 

	// messages 테이블(m)과 users 테이블(u)을 닉네임 기준으로 합침(LEFT JOIN)
	// 메세지 저장 당시의 색이 아니라, '현재 users 테이블에 있는 색'을 가져옴
	baseQuery := `
		SELECT 
			m.id, 
			m.content, 
			m.sender_pod, 
			m.sender_nick, 
			COALESCE(u.color_code, '#ffffff'), -- 색 없으면 흰색
			to_char(m.created_at, 'HH24:MI:SS') 
		FROM messages m
		LEFT JOIN users u ON m.sender_nick = u.nickname
	`

	var rows *sql.Rows
	var err error

	if beforeIDStr != "" {
		beforeID, _ := strconv.Atoi(beforeIDStr)
		query := baseQuery + " WHERE m.id < $1 ORDER BY m.id DESC LIMIT $2"
		rows, err = db.Query(query, beforeID, limit)
	} else {
		query := baseQuery + " ORDER BY m.id DESC LIMIT $1"
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
		// Scan 순서: id, content, pod, nick, color(JOIN된 것), time
		rows.Scan(&m.ID, &m.Content, &m.SenderPod, &m.SenderNick, &m.SenderColor, &m.Time)
		history = append(history, m)
	}
	
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

	// 1. Users 테이블에 사용자 색상 정보 저장/업데이트 (가장 최신 색상 유지)
	_, err := db.Exec(`
		INSERT INTO users (nickname, color_code) VALUES ($1, $2)
		ON CONFLICT (nickname) DO UPDATE SET color_code = $2`, 
		nickname, color)
	if err != nil { log.Println("User Update Error:", err) }

	// 2. Messages 테이블 저장 (색상 컬럼 없음! 내용만 저장)
	var id int
	err = db.QueryRow(
		"INSERT INTO messages (content, sender_pod, sender_nick) VALUES ($1, $2, $3) RETURNING id",
		content, hostname, nickname,
	).Scan(&id)
	
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// 3. NATS 전송
	// 실시간 전송 시에는 JOIN을 할 수 없으니, 방금 받은 정보를 그대로 실어서 보냄
	msg := Message{
		ID:          id,
		Content:     content,
		SenderPod:   hostname,
		SenderNick:  nickname,
		SenderColor: color, // NATS 메세지엔 색상을 담아서 보냄 (즉시 반영용)
		Time:        time.Now().Format("15:04:05"),
	}
	data, _ := json.Marshal(msg)
	nc.Publish("chat.global", data)
	w.WriteHeader(http.StatusOK)
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

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