package main

import (
	"log"
	"net/http"
	"time"
)

func main() {
	s := NewServer(ServerConfig{
		Upgrades:     []string{TRANSPORT_WEBSOCKET},
		PingInterval: 300,
		PingTimeout:  200,
		MaxPayload:   1e6,
	})

	s.SetOnConnectionHandler(func(ss *ServerSocket) {
		ss.SetOnMessageHandler(func(data Message) {
			log.Printf("Received message: %v\n", data)
			ss.Send(data.Type, data.Data)
		})
	})

	dumper := time.NewTicker(2 * time.Second)
	go func() {
		for range dumper.C {
			s.debugDump()
		}
	}()

	http.HandleFunc("/engine.io/", s.engineIOHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.URL)
	})
	log.Fatal(http.ListenAndServe("localhost:3000", nil))
}
