package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return SendControlContext(ctx, path, request)
}

func SendControlContext(ctx context.Context, path string, request ControlRequest) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	return sendControlContext(ctx, conn, request)
}

func sendControlContext(ctx context.Context, conn net.Conn, request ControlRequest) error {
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancellation()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	var response ControlResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	if !response.OK {
		if response.Error == "" {
			return errors.New("proxy rejected control request")
		}
		return fmt.Errorf("proxy: %s", response.Error)
	}
	return nil
}
