package main

import (
	"log"
	"net/http"

	eio "github.com/tinaxd/engineiogo/engine"
)

func main() {
	s := eio.NewServer(eio.ServerConfig{
		Upgrades:     []string{eio.TRANSPORT_WEBSOCKET},
		PingInterval: 300,
		PingTimeout:  200,
		MaxPayload:   1e6,
	})

	s.SetOnConnectionHandler(func(ss *eio.ServerSocket) {
		ss.SetOnMessageHandler(func(data eio.Message) {
			log.Printf("Received message: %v\n", data)
			ss.Send(data.Type, data.Data)
		})
	})

	// dumper := time.NewTicker(2 * time.Second)
	// go func() {
	// 	for range dumper.C {
	// 		s.debugDump()
	// 	}
	// }()

	http.HandleFunc("/engine.io/", s.EngineIOHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.URL)
	})
	log.Fatal(http.ListenAndServe("localhost:3000", nil))
}
