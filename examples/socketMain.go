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
		Auth interface{} `json:"token"`
	}

	s.Namespace("/").SetOnConnectionHandler(func(ss *sio.ServerSocket, handshake []byte) {
		log.Print("/ handshake string: " + string(handshake))
		var a auth
		if err := json.Unmarshal(handshake, &a); err == nil {
			ss.Emit("auth", []interface{}{
				a,
			})
		} else {
			ss.Emit("auth", []interface{}{map[string]interface{}{}})
		}

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
		log.Print("/custom handshake string: " + string(handshake))
		var a auth
		if err := json.Unmarshal(handshake, &a); err == nil {
			ss.Emit("auth", []interface{}{
				a,
			})
		} else {
			ss.Emit("auth", []interface{}{map[string]interface{}{}})
		}
	})

	http.HandleFunc("/socket.io/", s.HandleFunc)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.URL)
	})
	log.Fatal(http.ListenAndServe("localhost:3000", nil))
}
