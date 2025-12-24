package main

import (
  "database/sql"
  "encoding/json"
  "fmt"
  "log"
  "net/http"
  "os"
  "time"

  _ "github.com/lib/pq" // Postgres 드라이버
  "github.com/nats-io/nats.go"
)

var nc *nats.Conn
var db *sql.DB
var hostname string

type Message struct {
  ID        int    `json:"id"`
  Content   string `json:"content"`
  SenderPod string `json:"sender_pod"`
  Time      string `json:"time"`
}

func main() {
  // 1. 환경 설정
  hostname, _ = os.Hostname()
  initDB()   // DB 연결
  initNATS() // NATS 연결

  // 2. 웹 핸들러
  http.Handle("/", http.FileServer(http.Dir("./static")))
  http.HandleFunc("/stream", streamHandler)
  http.HandleFunc("/send", sendHandler)

  port := "8080"
  log.Printf("🥤 CoTalk Server started on %s (Pod: %s)", port, hostname)
  if err := http.ListenAndServe(":"+port, nil); err != nil {
    log.Fatal(err)
  }
}

func initDB() {
  // K8s Env에서 정보 가져오기
  dbHost := os.Getenv("DB_HOST")
  dbUser := os.Getenv("DB_USER")
  dbPwd := os.Getenv("DB_PASSWORD")
  dbName := "cotalk" // DB 이름 (기본값)

  // DB 연결 문자열 (SSL 모드 해제)
  connStr := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, dbUser, dbPwd, dbName)
  
  var err error
  db, err = sql.Open("postgres", connStr)
  if err != nil {
    log.Fatal("DB Open Error: ", err)
  }

  // 연결 테스트
  if err := db.Ping(); err != nil {
    log.Fatal("DB Ping Error: ", err)
  }
  log.Println("✅ Connected to PostgreSQL")

  // 테이블 생성 (없으면 자동 생성)
  schema := `
  CREATE TABLE IF NOT EXISTS messages (
    id SERIAL PRIMARY KEY,
    content TEXT,
    sender_pod TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
  );`
  if _, err := db.Exec(schema); err != nil {
    log.Fatal("Create Table Error: ", err)
  }
}

func initNATS() {
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
  if content == "" {
    return
  }

  // 1. DB에 저장 (INSERT)
  var id int
  err := db.QueryRow(
    "INSERT INTO messages (content, sender_pod) VALUES ($1, $2) RETURNING id",
    content, hostname,
  ).Scan(&id)
  
  if err != nil {
    log.Println("DB Insert Error:", err)
    http.Error(w, err.Error(), 500)
    return
  }

  // 2. NATS로 전송 (Publish)
  msg := Message{
    ID:        id,
    Content:   content,
    SenderPod: hostname,
    Time:      time.Now().Format("15:04:05"),
  }
  data, _ := json.Marshal(msg)
  nc.Publish("chat.global", data)

  w.WriteHeader(http.StatusOK)
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
  w.Header().Set("Content-Type", "text/event-stream")
  w.Header().Set("Cache-Control", "no-cache")
  w.Header().Set("Connection", "keep-alive")

  // [중요] 접속하자마자 과거 대화 50개 뿌려주기
  rows, err := db.Query("SELECT id, content, sender_pod, to_char(created_at, 'HH24:MI:SS') FROM messages ORDER BY id DESC LIMIT 50")
  if err == nil {
    // 최신순으로 가져왔으니 뒤집어서 보여주거나, UI가 알아서 하거나.
    // 여기서는 간단히 그냥 보냄 (UI 스크립트가 쌓아줌)
    // 순서를 맞추려면 배열에 담아서 역순 정렬해야 하지만, 일단 간단히!
    var history []Message
    for rows.Next() {
      var m Message
      rows.Scan(&m.ID, &m.Content, &m.SenderPod, &m.Time)
      history = append(history, m)
    }
    rows.Close()
    
    // 과거 메시지는 역순(오래된 것부터)으로 보내야 채팅창 위에서부터 쌓임
    for i := len(history) - 1; i >= 0; i-- {
      data, _ := json.Marshal(history[i])
      fmt.Fprintf(w, "data: %s\n\n", data)
    }
    w.(http.Flusher).Flush()
  }

  // 실시간 메시지 대기 (NATS)
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
