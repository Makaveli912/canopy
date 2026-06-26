package contract

import (
	"log"
	"net/http"
)

// StartRPCServer() launches the plugin's own HTTP server.
func (p *Plugin) StartRPCServer() {
	addr := ":8081"
	mux := http.NewServeMux()
	log.Printf("plugin RPC server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("plugin RPC server error: %v", err)
	}
}
