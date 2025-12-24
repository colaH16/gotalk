package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go"
)

// JetStreamContext 삭제 (nc만 사용)
var nc *nats.Conn
var hostname string

type Message struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	SenderPod string `json:"sender_pod"`
	Time      string `json:"time"`
}

func main() {
	// 1. 환경 설정
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	hostname, _ = os.Hostname()

	// 2. NATS 연결 (Core NATS)
	var err error
	nc, err = nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil {
		log.Fatal("NATS Connect Error: ", err)
	}
	defer nc.Close()
	
	log.Println("✅ Connected to NATS Core (Pub/Sub Mode)")

	// 3. JetStream 설정 단계 삭제 (Stream 생성 코드 삭제)

	// 4. 웹 핸들러 등록
	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/stream", streamHandler)
	http.HandleFunc("/send", sendHandler)

	port := "8080"
	log.Printf("🥤 CoTalk Server started on %s (Pod: %s)", port, hostname)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

// 메시지 전송 핸들러
func sendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	content := r.FormValue("msg")
	if content == "" {
		return
	}

	msg := Message{
		Content:   content,
		SenderPod: hostname,
		Time:      time.Now().Format("15:04:05"),
	}
	data, _ := json.Marshal(msg)

	// [변경] js.Publish -> nc.Publish (Core NATS)
	// 저장 없이 구독자들에게 바로 쏘고 끝냅니다.
	err := nc.Publish("chat.global", data)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// 실시간 스트림 핸들러 (SSE)
func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// [변경] js.SubscribeSync -> nc.SubscribeSync (Core NATS)
	// 옵션(DeliverAll) 같은 거 없습니다. 지금부터 오는 것만 듣습니다.
	sub, err := nc.SubscribeSync("chat.global")
	if err != nil {
		log.Println("Subscribe Error:", err)
		return
	}
	defer sub.Unsubscribe()

	// 클라이언트가 끊을 때 감지하기 위한 채널
	notify := r.Context().Done()

	for {
		select {
		case <-notify:
			// 브라우저 끄면 루프 종료
			return
		default:
			// 1초 기다리며 메시지 확인
			m, err := sub.NextMsg(1 * time.Second)
			if err == nats.ErrTimeout {
				fmt.Fprintf(w, ":keepalive\n\n")
				w.(http.Flusher).Flush()
				continue
			}
			if err != nil {
				// 연결 에러 시 종료
				return
			}

			// 메시지 전송
			fmt.Fprintf(w, "data: %s\n\n", string(m.Data))
			w.(http.Flusher).Flush()
		}
	}
}