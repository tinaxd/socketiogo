package main

import (
	"encoding/json"
	"log"
	"net/http"

	sio "github.com/tinaxd/socketiogo/socket"
)

func main() {
	s := sio.NewServer()

	// dumper := time.NewTicker(2 * time.Second)
	// go func() {
	// 	for range dumper.C {
	// 		s.debugDump()
	// 	}
	// }()

	type auth struct {
		Auth interface{} `json:"auth"`
	}

	s.Namespace("/").SetOnConnectionHandler(func(ss *sio.ServerSocket, handshake []byte) {
		var a auth
		if err := json.Unmarshal(handshake, &a); err != nil {
			log.Println("error unmarshaling handshake:", err)
			return
		}
		ss.Emit("auth", []interface{}{
			a.Auth,
		})

		ss.SetEventHandler("message", sio.CreateEventHandler(func(ev *sio.Message) {
			if err := ss.Emit("message-back", ev.Args); err != nil {
				log.Printf("message-back: %v", err)
			}
		}))

		ss.SetEventHandler("message-with-ack", sio.CreateEventHandler(func(ev *sio.Message) {
			if err := ev.Ack(ev.Args); err != nil {
				log.Printf("message-with-ack ack error: %v", err)
			}
		}))
	})

	s.Namespace("/custom").SetOnConnectionHandler(func(ss *sio.ServerSocket, handshake []byte) {
		var a auth
		if err := json.Unmarshal(handshake, &a); err != nil {
			log.Println("error unmarshaling handshake:", err)
			return
		}
		ss.Emit("auth", []interface{}{
			a.Auth,
		})
	})

	http.HandleFunc("/socket.io/", s.HandleFunc)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.URL)
	})
	log.Fatal(http.ListenAndServe("localhost:3000", nil))
}
