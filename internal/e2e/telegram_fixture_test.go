//go:build linux

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	telegramFixtureToken = "kenogram-telegram-fixture-token"
	telegramFixtureUser  = int64(424242)
	telegramFixtureFile  = "telegram-file-proof"
)

type telegramOutbound struct {
	Method    string
	Text      string
	MessageID int
}

type telegramFixture struct {
	*httptest.Server

	mu             sync.Mutex
	updates        []map[string]any
	outbound       []telegramOutbound
	nextUpdateID   int
	nextMessageID  int
	fileRequests   int
	methodRequests map[string]int
	changed        chan struct{}
}

func newTelegramFixture(t *testing.T, host string) *telegramFixture {
	t.Helper()
	fixture := &telegramFixture{
		nextUpdateID:   1,
		nextMessageID:  100,
		methodRequests: make(map[string]int),
		changed:        make(chan struct{}, 1),
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(fixture.serveHTTP))
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	fixture.Server = server
	return fixture
}

func (f *telegramFixture) enqueueText(text string) int {
	return f.enqueueTextFrom(telegramFixtureUser, text)
}

func (f *telegramFixture) enqueueTextFrom(userID int64, text string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	messageID := f.nextUpdateID
	f.updates = append(f.updates, map[string]any{
		"update_id": f.nextUpdateID,
		"message": map[string]any{
			"message_id": messageID,
			"date":       1_700_000_000,
			"from":       map[string]any{"id": userID, "is_bot": false, "first_name": "Kenogram", "username": "kenogram_fixture", "language_code": "en"},
			"chat":       map[string]any{"id": userID, "type": "private", "first_name": "Kenogram"},
			"text":       text,
		},
	})
	f.nextUpdateID++
	f.signal()
	return messageID
}

func (f *telegramFixture) enqueueDocument() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	messageID := f.nextUpdateID
	f.updates = append(f.updates, map[string]any{
		"update_id": f.nextUpdateID,
		"message": map[string]any{
			"message_id": messageID,
			"date":       1_700_000_000,
			"from":       map[string]any{"id": telegramFixtureUser, "is_bot": false, "first_name": "Kenogram", "username": "kenogram_fixture", "language_code": "en"},
			"chat":       map[string]any{"id": telegramFixtureUser, "type": "private", "first_name": "Kenogram"},
			"document": map[string]any{
				"file_id":        "fixture-file-id",
				"file_unique_id": "fixture-file-unique-id",
				"file_name":      "proof.txt",
				"mime_type":      "text/plain",
				"file_size":      len(telegramFixtureFile),
			},
		},
	})
	f.nextUpdateID++
	f.signal()
	return messageID
}

func (f *telegramFixture) signal() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

func (f *telegramFixture) waitOutbound(t *testing.T, timeout time.Duration, fragment string) telegramOutbound {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, message := range f.outbound {
			if strings.Contains(message.Text, fragment) {
				f.mu.Unlock()
				return message
			}
		}
		f.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("Telegram output did not contain %q; observed: %#v", fragment, f.outbound)
	return telegramOutbound{}
}

func (f *telegramFixture) waitForFileRequest(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		count := f.fileRequests
		f.mu.Unlock()
		if count > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Engram never downloaded the Telegram fixture file")
}

func (f *telegramFixture) serveHTTP(response http.ResponseWriter, request *http.Request) {
	methodPrefix := "/telegram/bot" + telegramFixtureToken + "/"
	filePath := "/telegram/file/bot" + telegramFixtureToken + "/documents/proof.txt"
	if request.URL.Path == filePath {
		f.mu.Lock()
		f.fileRequests++
		f.mu.Unlock()
		response.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(response, telegramFixtureFile)
		return
	}
	if !strings.HasPrefix(request.URL.Path, methodPrefix) {
		http.NotFound(response, request)
		return
	}
	method := strings.TrimPrefix(request.URL.Path, methodPrefix)
	f.mu.Lock()
	f.methodRequests[method]++
	f.mu.Unlock()
	switch method {
	case "getMe":
		writeTelegramResult(response, map[string]any{
			"id":         7_770_001,
			"is_bot":     true,
			"first_name": "Kenogram Fixture",
			"username":   "kenogram_fixture_bot",
		})
	case "getUpdates":
		f.serveUpdates(response, request)
	case "getFile":
		writeTelegramResult(response, map[string]any{
			"file_id":   "fixture-file-id",
			"file_size": len(telegramFixtureFile),
			"file_path": "documents/proof.txt",
		})
	case "getWebhookInfo":
		writeTelegramResult(response, map[string]any{"url": "", "has_custom_certificate": false, "pending_update_count": 0})
	case "deleteWebhook", "deleteMyCommands", "setMyCommands", "pinChatMessage", "unpinChatMessage", "deleteMessage", "answerCallbackQuery":
		writeTelegramResult(response, true)
	default:
		f.serveOutbound(response, request, method)
	}
}

func (f *telegramFixture) serveUpdates(response http.ResponseWriter, request *http.Request) {
	_ = request.ParseForm()
	offset, _ := strconv.Atoi(request.Form.Get("offset"))
	updates := f.updatesAtOrAfter(offset)
	if len(updates) == 0 {
		select {
		case <-f.changed:
		case <-request.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		updates = f.updatesAtOrAfter(offset)
	}
	writeTelegramResult(response, updates)
}

func (f *telegramFixture) updatesAtOrAfter(offset int) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	updates := make([]map[string]any, 0, len(f.updates))
	for _, update := range f.updates {
		id, _ := update["update_id"].(int)
		if id >= offset {
			updates = append(updates, update)
		}
	}
	return updates
}

func (f *telegramFixture) serveOutbound(response http.ResponseWriter, request *http.Request, method string) {
	text, chatID, messageID := telegramRequestFields(request)
	f.mu.Lock()
	if chatID == 0 {
		chatID = telegramFixtureUser
	}
	if messageID == 0 {
		messageID = f.nextMessageID
		f.nextMessageID++
	}
	f.outbound = append(f.outbound, telegramOutbound{Method: method, Text: text, MessageID: messageID})
	f.mu.Unlock()
	writeTelegramResult(response, map[string]any{
		"message_id": messageID,
		"date":       1_700_000_000,
		"from":       map[string]any{"id": 7_770_001, "is_bot": true, "first_name": "Kenogram Fixture", "username": "kenogram_fixture_bot"},
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"text":       text,
	})
}

func telegramRequestFields(request *http.Request) (string, int64, int) {
	contentType := request.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var body map[string]any
		if json.NewDecoder(request.Body).Decode(&body) == nil {
			text, _ := body["text"].(string)
			if text == "" {
				text, _ = body["caption"].(string)
			}
			return text, numberInt64(body["chat_id"]), int(numberInt64(body["message_id"]))
		}
		return "", telegramFixtureUser, 0
	}
	_ = request.ParseMultipartForm(1 << 20)
	if request.MultipartForm != nil {
		return request.FormValue("caption"), parseInt64(request.FormValue("chat_id")), 0
	}
	_ = request.ParseForm()
	return request.Form.Get("text"), parseInt64(request.Form.Get("chat_id")), 0
}

func numberInt64(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case json.Number:
		result, _ := value.Int64()
		return result
	default:
		return 0
	}
}

func parseInt64(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return telegramFixtureUser
	}
	return parsed
}

func writeTelegramResult(response http.ResponseWriter, result any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{"ok": true, "result": result})
}

func telegramAPIBase(rawURL, host string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("parse fixture URL: %v", err))
	}
	return "http://" + net.JoinHostPort(host, parsed.Port()) + "/telegram"
}

func (f *telegramFixture) waitForMethod(t *testing.T, timeout time.Duration, method string) {
	t.Helper()
	f.waitForMethodAfter(t, timeout, method, 0)
}

func (f *telegramFixture) methodCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methodRequests[method]
}

func (f *telegramFixture) waitForMethodAfter(t *testing.T, timeout time.Duration, method string, previous int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.methodCount(method) > previous {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Telegram method %s did not advance beyond %d calls", method, previous)
}
