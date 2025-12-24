package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
)

var (
	nc       *nats.Conn
	db       *sql.DB
	hostname string
	
	// [수정] 채널 버퍼를 늘려 막힘 방지
	clients   = make(map[chan string]bool)
	broadcast = make(chan string, 100) 
	mutex     = sync.Mutex{}
)

// (Message, User 구조체는 동일)
type Message struct {
	ID          int    `json:"id"`
	Content     string `json:"content"`
	SenderPod   string `json:"sender_pod"`
	SenderNick  string `json:"sender_nick"`
	SenderColor string `json:"sender_color"`
	Time        string `json:"time"`
}

type User struct {
	Nickname  string `json:"nickname"`
	ColorCode string `json:"color_code"`
}

type User struct {
	Nickname  string `json:"nickname"`
	ColorCode string `json:"color_code"`
}

func main() {
	hostname, _ = os.Hostname()
	initDB()
	initNATS()

	go handleMessages()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/stream", streamHandler)
	http.HandleFunc("/send", sendHandler)
	http.HandleFunc("/history", historyHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/update", updateProfileHandler)

	port := "8080"
	log.Printf("🥤 CoTalk Server started on %s (Pod: %s)", port, hostname)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

// [방송실] NATS에서 받은 메시지를 현재 접속한 모든 사용자에게 전달
func handleMessages() {
	for {
		msg := <-broadcast
		// [로그] 방송실이 메시지를 수신했는지 확인
		log.Printf("📢 [Broadcaster] Broadcasting message to clients...")
		
		mutex.Lock()
		count := 0
		for clientChan := range clients {
			select {
			case clientChan <- msg:
				count++
			default:
			}
		}
		mutex.Unlock()
		log.Printf("✅ [Broadcaster] Sent to %d clients.", count)
	}
}

func initNATS() {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" { 
		natsURL = nats.DefaultURL 
		log.Println("⚠️ Warning: NATS_URL not set. Using default: " + natsURL)
	} else {
		log.Println("🔗 Connecting to NATS at: " + natsURL)
	}
	
	var err error
	nc, err = nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil { log.Fatal("❌ NATS Connect Error: ", err) }
	
	// [로그] NATS 구독 확인
	nc.Subscribe("chat.global", func(m *nats.Msg) {
		log.Printf("📨 [NATS Listener] Received msg from NATS: %s", string(m.Data))
		broadcast <- string(m.Data)
	})
	
	log.Println("✅ Connected to NATS & Listening (Hub Mode)...")
}

// [스트림 핸들러] 사용자가 웹소켓(SSE) 연결을 요청할 때
func streamHandler(w http.ResponseWriter, r *http.Request) {
	// 닉네임 파싱 (로그용)
	nick := r.URL.Query().Get("nick")
	if nick == "" { nick = "Unknown" }

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 내 전용 채널 생성 및 등록
	myChan := make(chan string, 10)
	
	mutex.Lock()
	clients[myChan] = true
	mutex.Unlock()

	// [로그] 접속 알림
	log.Printf("🔌 Connected: User [%s] attached to Pod [%s]", nick, hostname)

	// 연결 종료 시 처리 (defer)
	defer func() {
		mutex.Lock()
		delete(clients, myChan) // 명부에서 삭제
		close(myChan)           // 채널 닫기
		mutex.Unlock()
		
		// [로그] 퇴장 알림
		log.Printf("❌ Disconnected: User [%s] detached from Pod [%s]", nick, hostname)
	}()

	notify := r.Context().Done()

	for {
		select {
		case <-notify: // 브라우저 종료 시
			return
		case msg := <-myChan: // 방송실에서 메시지 도착
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		case <-time.After(15 * time.Second): // 15초간 조용하면 생존신고
			fmt.Fprintf(w, ":keepalive\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func initDB() {
	dbHost := os.Getenv("DB_HOST")
	dbUser := os.Getenv("DB_USER")
	dbPwd := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	if dbName == "" { dbName = "cotalk" }

	psqlInfo := fmt.Sprintf("host=%s user=%s password=%s dbname=postgres sslmode=disable", dbHost, dbUser, dbPwd)
	tempDB, err := sql.Open("postgres", psqlInfo)
	if err != nil { log.Fatal(err) }
	var exists bool
	tempDB.QueryRow("SELECT EXISTS(SELECT datname FROM pg_catalog.pg_database WHERE datname = $1)", dbName).Scan(&exists)
	if !exists { tempDB.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName)) }
	tempDB.Close()

	connStr := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, dbUser, dbPwd, dbName)
	db, err = sql.Open("postgres", connStr)
	if err != nil { log.Fatal(err) }
	
	// 테이블 생성 (기존 유지)
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

func loginHandler(w http.ResponseWriter, r *http.Request) {
	nick := r.URL.Query().Get("nick")
	var color string
	err := db.QueryRow("SELECT color_code FROM users WHERE nickname = $1", nick).Scan(&color)
	
	resp := User{Nickname: nick}
	if err == nil { resp.ColorCode = color }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func updateProfileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { return }
	nickname := r.FormValue("nick")
	color := r.FormValue("color")
	if nickname == "" { return }
	if color == "" { color = "#ffffff" }

	_, err := db.Exec(`
		INSERT INTO users (nickname, color_code) VALUES ($1, $2)
		ON CONFLICT (nickname) DO UPDATE SET color_code = $2`, 
		nickname, color)
	if err != nil { http.Error(w, err.Error(), 500); return }
	w.WriteHeader(http.StatusOK)
}

func historyHandler(w http.ResponseWriter, r *http.Request) {
	beforeIDStr := r.URL.Query().Get("before_id")
	limit := 30 
	baseQuery := `
		SELECT 
			m.id, m.content, m.sender_pod, m.sender_nick, 
			COALESCE(u.color_code, '#ffffff'), to_char(m.created_at, 'HH24:MI:SS') 
		FROM messages m
		LEFT JOIN users u ON m.sender_nick = u.nickname
	`

	var rows *sql.Rows
	var err error

	if beforeIDStr != "" {
		// [여기서 strconv 사용됨]
		beforeID, _ := strconv.Atoi(beforeIDStr)
		query := baseQuery + " WHERE m.id < $1 ORDER BY m.id DESC LIMIT $2"
		rows, err = db.Query(query, beforeID, limit)
	} else {
		query := baseQuery + " ORDER BY m.id DESC LIMIT $1"
		rows, err = db.Query(query, limit)
	}

	if err != nil { http.Error(w, err.Error(), 500); return }
	defer rows.Close()

	var history []Message
	for rows.Next() {
		var m Message
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

	// 1. 유저 정보 저장 (UPSERT)
	db.Exec(`
		INSERT INTO users (nickname, color_code) VALUES ($1, $2)
		ON CONFLICT (nickname) DO UPDATE SET color_code = $2`, 
		nickname, color)
	
	// 2. 메시지 저장
	var id int
	err := db.QueryRow(
		"INSERT INTO messages (content, sender_pod, sender_nick) VALUES ($1, $2, $3) RETURNING id",
		content, hostname, nickname,
	).Scan(&id)
	
	if err != nil { http.Error(w, err.Error(), 500); return }

	// 3. NATS로 전송 (이제 이건 서버들끼리만 듣는 방송)
	msg := Message{
		ID: id, Content: content, SenderPod: hostname, SenderNick: nickname, SenderColor: color,
		Time: time.Now().Format("15:04:05"),
	}
	data, _ := json.Marshal(msg)
	nc.Publish("chat.global", data)
	w.WriteHeader(http.StatusOK)
}