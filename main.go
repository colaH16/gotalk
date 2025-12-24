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

var js nats.JetStreamContext
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

	// 2. NATS 연결 (재시도 로직 포함)
	nc, err := nats.Connect(natsURL, nats.Name("GoTalk"), nats.MaxReconnects(-1))
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	// 3. JetStream 컨텍스트 생성 (데이터 저장을 위해 필수!)
	js, err = nc.JetStream()
	if err != nil {
		log.Fatal(err)
	}

	// 4. 스트림 생성 (채팅방 같은 저장소 개념, 없으면 만듦)
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "COTALK",
		Subjects: []string{"chat.>"},
		Storage:  nats.FileStorage, // 파일에 저장해야 Pod 죽어도 남음
	})
	if err != nil {
		log.Printf("Stream setup check: %v", err)
	}

	// 5. 웹 핸들러 등록
	http.Handle("/", http.FileServer(http.Dir("./static"))) // HTML 파일 서빙
	http.HandleFunc("/stream", streamHandler)               // 실시간 수신 (SSE)
	http.HandleFunc("/send", sendHandler)                   // 메시지 전송

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

	// NATS JetStream에 저장 (Publish)
	// chat.global이라는 주제로 보냄
	_, err := js.Publish("chat.global", data)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// 실시간 스트림 핸들러 (SSE)
func streamHandler(w http.ResponseWriter, r *http.Request) {
	// SSE 헤더 설정
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// NATS 구독 (지난 대화도 다 보내달라고 설정: DeliverAll)
	sub, err := js.SubscribeSync("chat.global", nats.DeliverAll())
	if err != nil {
		log.Println(err)
		return
	}
	defer sub.Unsubscribe()

	// 클라이언트 접속 끊길 때까지 루프
	for {
		// 1초 기다리며 메시지 확인
		m, err := sub.NextMsg(1 * time.Second)
		if err == nats.ErrTimeout {
			// 메시지 없으면 빈 값 보내서 연결 유지 (Heartbeat)
			fmt.Fprintf(w, ":keepalive\n\n")
			w.(http.Flusher).Flush()
			continue
		}
		if err != nil {
			break
		}

		// 메시지 있으면 브라우저로 전송
		fmt.Fprintf(w, "data: %s\n\n", string(m.Data))
		w.(http.Flusher).Flush()
	}
}