package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	SenderNick string `json:"sender_nick"` // 닉네임 필드
	Time       string `json:"time"`
}

func main() {
	hostname, _ = os.Hostname()

	initDB()
	initNATS()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/stream", streamHandler)
	http.HandleFunc("/send", sendHandler)

	port := "8080"
	log.Printf("🥤 CoTalk Server started on %s (Pod: %s)", port, hostname)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

// ... initDB, initNATS 함수는 기존과 동일 ...
// (지면 관계상 생략, 기존 코드 그대로 두시면 됩니다)
func initDB() {
	// (기존 코드 유지)
    // ...
	dbHost := os.Getenv("DB_HOST")
	dbUser := os.Getenv("DB_USER")
	dbPwd := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "cotalk"
	}

	psqlInfo := fmt.Sprintf("host=%s user=%s password=%s dbname=postgres sslmode=disable", dbHost, dbUser, dbPwd)
	tempDB, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		log.Fatal("Temp DB Connection Error: ", err)
	}

	var exists bool
	err = tempDB.QueryRow("SELECT EXISTS(SELECT datname FROM pg_catalog.pg_database WHERE datname = $1)", dbName).Scan(&exists)
	if err != nil {
		log.Fatal("Check DB Error: ", err)
	}

	if !exists {
		log.Printf("Database '%s' does not exist. Creating...", dbName)
		_, err = tempDB.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName))
		if err != nil {
			log.Fatal("Create Database Error: ", err)
		}
		log.Println("✅ Database created successfully!")
	}
	tempDB.Close()

	connStr := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, dbUser, dbPwd, dbName)
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("DB Open Error: ", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatal("DB Ping Error: ", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS messages (
		id SERIAL PRIMARY KEY,
		content TEXT,
		sender_pod TEXT,
		sender_nick TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := db.Exec(schema); err != nil {
		log.Fatal("Create Table Error: ", err)
	}
}

func initNATS() {
	// (기존 코드 유지)
    natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	var err error
	nc, err = nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil {
		log.Fatal("NATS Error: ", err)
	}
	log.Println("✅ Connected to NATS Core")
}


func sendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	content := r.FormValue("msg")
	// [수정] 클라이언트가 보낸 닉네임을 받습니다.
	nickname := r.FormValue("nick")
	
	// 닉네임이 없으면 '익명' 처리 (혹은 파드명)
	if nickname == "" {
		nickname = "익명"
	}

	if content == "" {
		return
	}

	// 1. DB 저장
	var id int
	err := db.QueryRow(
		"INSERT INTO messages (content, sender_pod, sender_nick) VALUES ($1, $2, $3) RETURNING id",
		content, hostname, nickname,
	).Scan(&id)
	
	if err != nil {
		log.Println("DB Insert Error:", err)
		http.Error(w, err.Error(), 500)
		return
	}

	// 2. NATS 전송
	msg := Message{
		ID:         id,
		Content:    content,
		SenderPod:  hostname,
		SenderNick: nickname,
		Time:       time.Now().Format("15:04:05"),
	}
	data, _ := json.Marshal(msg)
	
	// 내가 보낸 메시지도 NATS를 통해 다시 나에게 돌아옵니다 (Echo).
	// 프론트엔드에서는 이걸 받아서 그립니다.
	nc.Publish("chat.global", data)

	w.WriteHeader(http.StatusOK)
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 1. 과거 대화
	rows, err := db.Query("SELECT id, content, sender_pod, sender_nick, to_char(created_at, 'HH24:MI:SS') FROM messages ORDER BY id DESC LIMIT 50")
	if err == nil {
		var history []Message
		for rows.Next() {
			var m Message
			rows.Scan(&m.ID, &m.Content, &m.SenderPod, &m.SenderNick, &m.Time)
			history = append(history, m)
		}
		rows.Close()
		
		for i := len(history) - 1; i >= 0; i-- {
			data, _ := json.Marshal(history[i])
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		w.(http.Flusher).Flush()
	}

	// 2. 실시간 대화
	sub, err := nc.SubscribeSync("chat.global")
	if err != nil {
		return
	}
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
			if err != nil {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", string(m.Data))
			w.(http.Flusher).Flush()
		}
	}
}