package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

type ControlRequest struct {
	Operation    string        `json:"operation"`
	Host         string        `json:"host,omitempty"`
	Port         int           `json:"port,omitempty"`
	Duration     string        `json:"duration,omitempty"`
	Destinations []Destination `json:"destinations,omitempty"`
}
type ControlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (p *Proxy) ServeControl(path string) error {
	os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go p.control(conn)
	}
}
func (p *Proxy) control(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var request ControlRequest
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
		json.NewEncoder(conn).Encode(ControlResponse{Error: "invalid request"})
		return
	}
	switch request.Operation {
	case "ping":
		json.NewEncoder(conn).Encode(ControlResponse{OK: true})
	case "grant":
		duration, err := time.ParseDuration(request.Duration)
		if err == nil {
			err = p.Grant(Destination{request.Host, request.Port}, duration)
		}
		if err != nil {
			json.NewEncoder(conn).Encode(ControlResponse{Error: err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(ControlResponse{OK: true})
	case "remove":
		if request.Host == "" || request.Port < 1 || request.Port > 65535 {
			json.NewEncoder(conn).Encode(ControlResponse{Error: "invalid destination"})
			return
		}
		p.Remove(Destination{request.Host, request.Port})
		json.NewEncoder(conn).Encode(ControlResponse{OK: true})
	case "reconcile":
		if err := p.Reconcile(request.Destinations); err != nil {
			json.NewEncoder(conn).Encode(ControlResponse{Error: err.Error()})
			return
		}
		json.NewEncoder(conn).Encode(ControlResponse{OK: true})
	default:
		json.NewEncoder(conn).Encode(ControlResponse{Error: "unsupported operation"})
	}
}
func SendControl(path string, request ControlRequest) error {
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	var response ControlResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("proxy: %s", response.Error)
	}
	return nil
}
