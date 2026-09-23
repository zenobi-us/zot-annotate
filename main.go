package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var uiFS embed.FS

const name = "zot-annotate"

var version = "0.1.0"

type config struct {
	// Host defaults to loopback. Set to 0.0.0.0 only on a trusted network.
	Host string `json:"host,omitempty"`
}

type app struct {
	mu         sync.RWMutex
	writeMu    sync.Mutex
	latest     string
	sessionID  string
	messageID  string
	generation uint64
	lastURL    string
	cfg        config
	dataDir    string
	server     *http.Server
	listener   net.Listener
}

type frame struct {
	Type         string   `json:"type"`
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name,omitempty"`
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Protocol     int      `json:"protocol_version,omitempty"`
	CWD          string   `json:"cwd,omitempty"`
	ExtensionDir string   `json:"extension_dir,omitempty"`
	DataDir      string   `json:"data_dir,omitempty"`
	Event        string   `json:"event,omitempty"`
	Events       []string `json:"events,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	MessageID    string   `json:"latest_assistant_message_id,omitempty"`
	Snapshot     string   `json:"latest_assistant_message,omitempty"`
	Description  string   `json:"description,omitempty"`
	Text         string   `json:"text,omitempty"`
	Display      string   `json:"display,omitempty"`
	Action       string   `json:"action,omitempty"`
	Error        string   `json:"error,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
}

func main() {
	a := &app{}
	if err := a.run(); err != nil {
		fmt.Fprintln(os.Stderr, "[zot-annotate]", err)
		os.Exit(1)
	}
}

func (a *app) run() error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if err := a.send(enc, frame{Type: "hello", Name: name, Version: version, Capabilities: []string{"commands", "events"}}); err != nil {
		return err
	}
	if !scanner.Scan() {
		return scanner.Err()
	}
	var ack frame
	if err := json.Unmarshal(scanner.Bytes(), &ack); err != nil {
		return fmt.Errorf("hello_ack: %w", err)
	}
	if ack.Type != "hello_ack" {
		return fmt.Errorf("expected hello_ack, got %q", ack.Type)
	}
	a.dataDir = ack.DataDir
	if a.dataDir == "" {
		a.dataDir = ack.ExtensionDir
	}
	if a.dataDir == "" {
		a.dataDir = "."
	}
	a.cfg = loadConfig(a.dataDir)
	if err := a.send(enc, frame{Type: "register_command", Name: "annotate", Description: "open a browser editor to annotate the latest agent message"}); err != nil {
		return err
	}
	if err := a.send(enc, frame{Type: "subscribe", Events: []string{"assistant_message", "session_start", "session_end", "session_snapshot"}}); err != nil {
		return err
	}
	if err := a.send(enc, frame{Type: "ready"}); err != nil {
		return err
	}
	for scanner.Scan() {
		var in frame
		if err := json.Unmarshal(scanner.Bytes(), &in); err != nil {
			continue
		}
		switch in.Type {
		case "event":
			a.handleEvent(in)
		case "command_invoked":
			if in.Name == "annotate" {
				a.handleCommand(enc, in.ID)
			}
		case "shutdown":
			_ = a.send(enc, frame{Type: "shutdown_ack"})
			return nil
		}
	}
	return scanner.Err()
}

func (a *app) handleEvent(in frame) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch in.Event {
	case "session_end":
		// Historical assistant_message events are not replayed when Zot
		// switches sessions. Never let the previous session remain annotatable.
		a.latest = ""
		a.sessionID = ""
		a.messageID = ""
		a.generation++

	case "session_start":
		a.sessionID = in.SessionID
		a.latest = ""
		a.messageID = ""
		a.generation++
		// Accept the snapshot fields when newer Zot versions include them on
		// session_start, while remaining compatible with older hosts.
		if in.Snapshot != "" {
			a.latest = in.Snapshot
			a.messageID = in.MessageID
		}

	case "session_snapshot":
		// This is the authoritative state after session/tree navigation.
		a.sessionID = in.SessionID
		a.latest = in.Snapshot
		a.messageID = in.MessageID
		a.generation++

	case "assistant_message":
		if in.SessionID != "" && a.sessionID != "" && in.SessionID != a.sessionID {
			return
		}
		if strings.TrimSpace(in.Text) == "" {
			return
		}
		if a.sessionID == "" {
			a.sessionID = in.SessionID
		}
		if a.generation == 0 {
			a.generation = 1
		}
		a.latest = in.Text
		a.messageID = in.MessageID
	}
}

func (a *app) send(enc *json.Encoder, v frame) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return enc.Encode(v)
}

func loadConfig(dir string) config {
	c := config{Host: "127.0.0.1"}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if env := os.Getenv("ZOT_ANNOTATE_HOST"); env != "" {
		c.Host = env
	}
	if c.Host != "127.0.0.1" && c.Host != "0.0.0.0" && c.Host != "localhost" {
		c.Host = "127.0.0.1"
	}
	return c
}

func (a *app) handleCommand(enc *json.Encoder, id string) {
	a.mu.RLock()
	hasMessage := strings.TrimSpace(a.latest) != ""
	a.mu.RUnlock()
	if !hasMessage {
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "No assistant message is available to annotate yet."})
		return
	}
	if err := a.startServer(); err != nil {
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Could not start annotation UI: " + err.Error()})
		return
	}
	a.mu.RLock()
	url := a.lastURL
	a.mu.RUnlock()
	openBrowser(url)
	_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Annotation UI opened in your browser. Submit it there to send feedback to this session."})
}

func (a *app) startServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(a.cfg.Host, "0"))
	if err != nil {
		return err
	}
	tmpl, err := template.ParseFS(uiFS, "ui/index.html")
	if err != nil {
		_ = ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		msg := a.latest
		a.mu.RUnlock()
		_ = tmpl.Execute(w, map[string]string{"Message": msg})
	})
	mux.HandleFunc("/api/state", a.state)
	mux.HandleFunc("/api/upload", a.upload)
	mux.HandleFunc("/api/submit", a.submit)
	a.listener, a.server = ln, &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	a.lastURL = "http://" + net.JoinHostPort(browserHost(a.cfg.Host), portOf(ln))
	go func() {
		if err := a.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "[zot-annotate] web:", err)
		}
	}()
	return nil
}

func browserHost(host string) string {
	if host == "0.0.0.0" {
		return "127.0.0.1"
	}
	return host
}
func portOf(ln net.Listener) string { return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port) }

func (a *app) state(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"message":    a.latest,
		"session_id": a.sessionID,
		"message_id": a.messageID,
		"generation": a.generation,
	})
}

func (a *app) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var paths []string
	for _, hs := range r.MultipartForm.File["files"] {
		if p, err := saveUpload(hs, a.dataDir); err == nil {
			paths = append(paths, p)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"paths": paths})
}
func saveUpload(h *multipart.FileHeader, dir string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	d := filepath.Join(dir, "annotations")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	in, err := h.Open()
	if err != nil {
		return "", err
	}
	defer in.Close()
	f, err := os.CreateTemp(d, "attachment-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(in, 25<<20))
	return f.Name(), err
}

func (a *app) submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in struct {
		Annotations json.RawMessage `json:"annotations"`
		Attachments []string        `json:"attachments"`
		Generation  uint64          `json:"generation"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&in); err != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	a.mu.RLock()
	message := a.latest
	generation := a.generation
	a.mu.RUnlock()
	if in.Generation == 0 || in.Generation != generation {
		http.Error(w, "annotation target changed; reopen /annotate for the current session", http.StatusConflict)
		return
	}
	if strings.TrimSpace(message) == "" {
		http.Error(w, "no assistant message is available to annotate", http.StatusConflict)
		return
	}
	prompt := "Please revise your previous response using this user annotation feedback.\n\nANNOTATED MESSAGE:\n" + message + "\n\nFEEDBACK (JSON):\n" + string(in.Annotations)
	if len(in.Attachments) > 0 {
		prompt += "\n\nATTACHED FILES (use the read tool to inspect them):\n- " + strings.Join(in.Attachments, "\n- ")
	}
	if err := a.submitToSession(prompt); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) submitToSession(text string) error {
	b, err := json.Marshal(frame{Type: "submit", Text: text})
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
