package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/google/uuid"
)

const (
	TRANSPORT_POLLING   = "polling"
	TRANSPORT_WEBSOCKET = "websocket"
)

type packetType int

const (
	PACKET_TYPE_OPEN packetType = 0
)

type Server struct {
	Config ServerConfig
}

type ServerConfig struct {
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
	MaxPayload   int      `json:"maxPayload"`
}

type openPacketResponse struct {
	Sid string `json:"sid"`
	ServerConfig
}

func (s *Server) engineIOHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	v := r.URL.Query()
	log.Println(v)

	eio := v.Get("EIO")
	if eio != "4" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	transport := v.Get("transport")
	if transport == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if transport != TRANSPORT_POLLING && transport != TRANSPORT_WEBSOCKET {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sidUuid, err := uuid.NewRandom()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	sid := sidUuid.String()

	response := openPacketResponse{
		Sid:          sid,
		ServerConfig: s.Config,
	}
	responseText, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("charset", "UTF-8")

	res := fmt.Sprintf("%d%s", PACKET_TYPE_OPEN, responseText)
	w.Write([]byte(res))
}

func main() {
	s := &Server{
		Config: ServerConfig{
			Upgrades:     []string{TRANSPORT_WEBSOCKET},
			PingInterval: 300,
			PingTimeout:  200,
			MaxPayload:   1e6,
		}}
	http.HandleFunc("/engine.io/", s.engineIOHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.URL)
	})
	log.Fatal(http.ListenAndServe("localhost:3000", nil))
}
