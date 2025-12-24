package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
)

var (
	nc       *nats.Conn
	db       *sql.DB
	hostname string

	// [핵심] 사용자 관리용 Hub
	// 접속한 클라이언트들의 채널을 보관하는 명부
	clients   = make(map[chan string]bool) 
	broadcast = make(chan string)           // NATS에서 받은 메시지를 뿌리는 파이프
	mutex     = sync.Mutex{}                // 명부 작성할 때 충돌 방지용 자물쇠
)

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

func main() {
	hostname, _ = os.Hostname()
	initDB()
	initNATS()

	// [중요] 방송실 가동 (고루틴)
	// 들어오는 메시지를 모든 클라이언트에게 배달하는 역할
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

// [핵심 로직] 방송실: NATS에서 온 메시지를 접속자 전원에게 쏜다
func handleMessages() {
	for {
		// 1. 방송 파이프에서 메시지 하나 꺼냄
		msg := <-broadcast
		
		// 2. 명부(clients)를 펼침 (자물쇠 잠그고)
		mutex.Lock()
		for clientChan := range clients {
			// 3. 각 사용자에게 메시지 전송 (Non-blocking)
			// 듣지 않는 사용자가 있어도 멈추지 않고 패스함
			select {
			case clientChan <- msg:
			default:
				// 너무 느린 사용자는 명부에서 지울 수도 있음 (여기선 생략)
			}
		}
		mutex.Unlock()
	}
}

func initNATS() {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" { natsURL = nats.DefaultURL }
	
	var err error
	nc, err = nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil { log.Fatal(err) }
	
	// [변경] 비동기 구독 (Async Subscribe)
	// 메시지가 오면 즉시 broadcast 채널로 던져버림
	nc.Subscribe("chat.global", func(m *nats.Msg) {
		broadcast <- string(m.Data)
	})
	
	log.Println("✅ Connected to NATS & Listening...")
}

// [변경] 스트림 핸들러: NATS 구독 안 함 -> Hub에 등록만 함
func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 1. 내 전용 채널 생성
	myChan := make(chan string, 10) // 버퍼를 줘서 약간의 여유를 둠

	// 2. 명부에 등록 (입장)
	mutex.Lock()
	clients[myChan] = true
	mutex.Unlock()

	// 3. 나가면 명부에서 삭제 (퇴장)
	defer func() {
		mutex.Lock()
		delete(clients, myChan)
		close(myChan)
		mutex.Unlock()
	}()

	notify := r.Context().Done()

	for {
		select {
		case <-notify:
			return // 브라우저 끄면 종료
		case msg := <-myChan:
			// 4. 방송실에서 내 채널로 넣어준 메시지를 화면에 씀
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		case <-time.After(15 * time.Second):
			// 5. 15초간 조용하면 생존신고 (KeepAlive)
			fmt.Fprintf(w, ":keepalive\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

// --- 아래는 기존과 동일하거나 DB 관련 로직 ---

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
	if err := db.Ping(); err != nil { log.Fatal(err) }

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

	// DB 저장
	_, err := db.Exec(`
		INSERT INTO users (nickname, color_code) VALUES ($1, $2)
		ON CONFLICT (nickname) DO UPDATE SET color_code = $2`, 
		nickname, color)
	
	var id int
	err = db.QueryRow(
		"INSERT INTO messages (content, sender_pod, sender_nick) VALUES ($1, $2, $3) RETURNING id",
		content, hostname, nickname,
	).Scan(&id)
	
	if err != nil { http.Error(w, err.Error(), 500); return }

	// NATS 전송
	msg := Message{
		ID: id, Content: content, SenderPod: hostname, SenderNick: nickname, SenderColor: color,
		Time: time.Now().Format("15:04:05"),
	}
	data, _ := json.Marshal(msg)
	nc.Publish("chat.global", data)
	w.WriteHeader(http.StatusOK)
}