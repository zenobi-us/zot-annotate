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
	"strconv"
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

type theme struct {
	Background string
	Foreground string
	Muted      string
	Accent     string
	Assistant  string
	Tool       string
	Error      string
}

type agentMessage struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text"`
}

type pendingAnnotation struct {
	Text        string   `json:"text"`
	Comment     string   `json:"comment"`
	Attachments []string `json:"attachments,omitempty"`
}

type app struct {
	mu                 sync.RWMutex
	writeMu            sync.Mutex
	latest             string
	sessionID          string
	messageID          string
	generation         uint64
	lastURL            string
	cfg                config
	dataDir            string
	server             *http.Server
	listener           net.Listener
	themeStyle         string
	annotateActive     bool
	annotateMessages   []agentMessage
	messageHistory     []agentMessage
	annotateCursor     int
	pendingAnnotations []pendingAnnotation
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
	Args         string   `json:"args,omitempty"`
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
	a.themeStyle = themeCSS(loadTheme(a.dataDir))
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
				a.handleCommand(enc, in.ID, in.Args)
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
		a.annotateActive = false
		a.annotateMessages = nil
		a.messageHistory = nil
		a.annotateCursor = 0
		a.pendingAnnotations = nil

	case "session_start":
		a.sessionID = in.SessionID
		a.latest = ""
		a.messageID = ""
		a.generation++
		a.annotateActive = false
		a.annotateMessages = nil
		a.messageHistory = nil
		a.annotateCursor = 0
		a.pendingAnnotations = nil
		// Accept the snapshot fields when newer Zot versions include them on
		// session_start, while remaining compatible with older hosts.
		if in.Snapshot != "" {
			a.latest = in.Snapshot
			a.messageID = in.MessageID
			a.messageHistory = append(a.messageHistory, agentMessage{ID: in.MessageID, Text: in.Snapshot})
		}

	case "session_snapshot":
		// This is the authoritative state after session/tree navigation.
		a.sessionID = in.SessionID
		a.latest = in.Snapshot
		a.messageID = in.MessageID
		a.generation++
		a.annotateActive = false
		a.annotateMessages = nil
		a.messageHistory = nil
		a.annotateCursor = 0
		a.pendingAnnotations = nil
		if strings.TrimSpace(in.Snapshot) != "" {
			a.messageHistory = append(a.messageHistory, agentMessage{ID: in.MessageID, Text: in.Snapshot})
		}

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
		a.messageHistory = append(a.messageHistory, agentMessage{ID: in.MessageID, Text: in.Text})
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

func loadTheme(dataDir string) theme {
	t := theme{Background: "#080a0d", Foreground: "#dadada", Muted: "#808080", Accent: "#6adaff", Assistant: "#87d7ff", Tool: "#87d787", Error: "#ff5f5f"}
	paths := []string{}
	if explicit := os.Getenv("ZOT_ANNOTATE_THEME_FILE"); explicit != "" {
		paths = append(paths, explicit)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			paths = append(paths, themePathFromConfig(cfg)...)
		}
	}
	zotHome := os.Getenv("ZOT_HOME")
	if zotHome == "" {
		zotHome = os.Getenv("XDG_STATE_HOME")
		if zotHome != "" {
			zotHome = filepath.Join(zotHome, "zot")
		} else if home, err := os.UserHomeDir(); err == nil {
			zotHome = filepath.Join(home, ".local", "state", "zot")
		}
	}
	if b, err := os.ReadFile(filepath.Join(zotHome, "config.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			paths = append(paths, themePathFromConfig(cfg)...)
		}
	}
	for _, path := range paths {
		if loaded, ok := readTheme(path, zotHome); ok {
			return mergeTheme(t, loaded)
		}
	}
	return t
}

func themePathFromConfig(cfg map[string]any) []string {
	var paths []string
	for _, key := range []string{"theme_file", "theme_path", "color_theme", "theme"} {
		if value, ok := cfg[key].(string); ok && value != "" && value != "auto" && value != "dark" && value != "light" {
			paths = append(paths, value)
		}
	}
	return paths
}

func readTheme(path, zotHome string) (theme, bool) {
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		candidates = append(candidates, filepath.Join(zotHome, "themes", path), filepath.Join(zotHome, "themes", path+".json"))
	}
	for _, candidate := range candidates {
		b, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(b, &raw) != nil {
			continue
		}
		colors, _ := raw["colors"].(map[string]any)
		if dark, ok := colors["dark"].(map[string]any); ok {
			colors = dark
		}
		return theme{Background: colorValue(colors["background"]), Foreground: colorValue(colors["fg"]), Muted: colorValue(colors["muted"]), Accent: colorValue(colors["accent"]), Assistant: colorValue(colors["assistant"]), Tool: colorValue(colors["tool"]), Error: colorValue(colors["error"])}, true
	}
	return theme{}, false
}

func mergeTheme(base, override theme) theme {
	if override.Background != "" {
		base.Background = override.Background
	}
	if override.Foreground != "" {
		base.Foreground = override.Foreground
	}
	if override.Muted != "" {
		base.Muted = override.Muted
	}
	if override.Accent != "" {
		base.Accent = override.Accent
	}
	if override.Assistant != "" {
		base.Assistant = override.Assistant
	}
	if override.Tool != "" {
		base.Tool = override.Tool
	}
	if override.Error != "" {
		base.Error = override.Error
	}
	return base
}

func colorValue(value any) string {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "#") {
			return v
		}
	case float64:
		return xtermColor(int(v))
	case map[string]any:
		r, rok := v["r"].(float64)
		g, gok := v["g"].(float64)
		b, bok := v["b"].(float64)
		if rok && gok && bok {
			return fmt.Sprintf("#%02x%02x%02x", int(r), int(g), int(b))
		}
	}
	return ""
}

func xtermColor(index int) string {
	if index < 0 || index > 255 {
		return ""
	}
	if index < 16 {
		palette := []string{"#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0", "#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff"}
		return palette[index]
	}
	if index >= 232 {
		value := 8 + (index-232)*10
		return fmt.Sprintf("#%02x%02x%02x", value, value, value)
	}
	levels := []int{0, 95, 135, 175, 215, 255}
	n := index - 16
	return fmt.Sprintf("#%02x%02x%02x", levels[n/36], levels[(n/6)%6], levels[n%6])
}

func themeCSS(t theme) string {
	return fmt.Sprintf(":root{--zot-theme-bg:%s;--zot-theme-fg:%s;--zot-theme-muted:%s;--zot-theme-accent:%s;--zot-theme-assistant:%s;--zot-theme-tool:%s;--zot-theme-error:%s}", t.Background, t.Foreground, t.Muted, t.Accent, t.Assistant, t.Tool, t.Error)
}

func (a *app) syncAnnotations() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.annotateActive {
		return false
	}
	a.annotateMessages = append(a.annotateMessages, a.messageHistory[a.annotateCursor:]...)
	a.annotateCursor = len(a.messageHistory)
	return true
}

func (a *app) handleCommand(enc *json.Encoder, id string, args string) {
	subcommand := ""
	if fields := strings.Fields(strings.TrimSpace(args)); len(fields) > 0 {
		subcommand = strings.ToLower(fields[0])
	}
	switch subcommand {
	case "cancel":
		a.cancelAnnotations()
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Annotation session cancelled; pending annotations discarded."})
		return
	case "collect":
		if err := a.collectAnnotations(); err != nil {
			_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Could not collect annotations: " + err.Error()})
			return
		}
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Collected pending annotations and sent them to the current session."})
		return
	case "sync":
		active := a.syncAnnotations()
		if !active {
			_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "No annotation session is active. Run /annotate first."})
			return
		}
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Annotation session synced; new agent messages are available in the browser."})
		return
	}

	a.mu.Lock()
	if strings.TrimSpace(a.latest) == "" {
		a.mu.Unlock()
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "No assistant message is available to annotate yet."})
		return
	}
	a.annotateActive = true
	a.annotateMessages = []agentMessage{{ID: a.messageID, Text: a.latest}}
	a.annotateCursor = len(a.messageHistory)
	a.pendingAnnotations = nil
	a.mu.Unlock()
	if err := a.startServer(); err != nil {
		_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Could not start annotation UI: " + err.Error()})
		return
	}
	a.mu.RLock()
	url := a.lastURL
	a.mu.RUnlock()
	openBrowser(url)
	_ = a.send(enc, frame{Type: "command_response", ID: id, Action: "display", Display: "Annotation UI opened in your browser. Use /annotate sync, /annotate collect, or /annotate cancel as needed."})
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
		_ = tmpl.Execute(w, map[string]any{"Message": msg, "ThemeStyle": template.CSS(a.themeStyle)})
	})
	mux.HandleFunc("/api/state", a.state)
	mux.HandleFunc("/api/annotations", a.annotations)
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
		"message":       a.latest,
		"messages":      a.annotateMessages,
		"session_id":    a.sessionID,
		"message_id":    a.messageID,
		"generation":    a.generation,
		"active":        a.annotateActive,
		"pending_count": len(a.pendingAnnotations),
	})
}

func (a *app) annotations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.RLock()
		pending := append([]pendingAnnotation(nil), a.pendingAnnotations...)
		a.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"annotations": pending})
	case http.MethodPost:
		var in struct {
			Annotation pendingAnnotation `json:"annotation"`
			Generation uint64            `json:"generation"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Annotation.Text) == "" || strings.TrimSpace(in.Annotation.Comment) == "" {
			http.Error(w, "invalid annotation", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if !a.annotateActive || in.Generation == 0 || in.Generation != a.generation {
			http.Error(w, "annotation session changed; reopen /annotate", http.StatusConflict)
			return
		}
		a.pendingAnnotations = append(a.pendingAnnotations, in.Annotation)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		index, err := strconv.Atoi(r.URL.Query().Get("index"))
		if err != nil {
			http.Error(w, "invalid annotation index", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if index < 0 || index >= len(a.pendingAnnotations) {
			http.Error(w, "annotation not found", http.StatusNotFound)
			return
		}
		a.pendingAnnotations = append(a.pendingAnnotations[:index], a.pendingAnnotations[index+1:]...)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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

func (a *app) cancelAnnotations() {
	a.mu.Lock()
	server := a.server
	a.server = nil
	a.listener = nil
	a.annotateActive = false
	a.annotateMessages = nil
	a.pendingAnnotations = nil
	a.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

func (a *app) collectAnnotations() error {
	a.mu.RLock()
	active := a.annotateActive
	pending := append([]pendingAnnotation(nil), a.pendingAnnotations...)
	message := a.latest
	a.mu.RUnlock()
	if !active {
		return fmt.Errorf("no annotation session is active")
	}
	if len(pending) == 0 {
		return nil
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("no assistant message is available to annotate")
	}
	payload, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	prompt := "Please revise your previous response using the annotation feedback collected so far.\n\nANNOTATED MESSAGE:\n" + message + "\n\nFEEDBACK (JSON):\n" + string(payload)
	if err := a.submitToSession(prompt); err != nil {
		return err
	}
	a.mu.Lock()
	a.pendingAnnotations = nil
	a.mu.Unlock()
	return nil
}

func (a *app) submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var in struct {
		Annotations []pendingAnnotation `json:"annotations"`
		Attachments []string            `json:"attachments"`
		Generation  uint64              `json:"generation"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&in); err != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	a.mu.RLock()
	message := a.latest
	generation := a.generation
	pending := append([]pendingAnnotation(nil), a.pendingAnnotations...)
	a.mu.RUnlock()
	if in.Generation == 0 || in.Generation != generation {
		http.Error(w, "annotation target changed; reopen /annotate for the current session", http.StatusConflict)
		return
	}
	if strings.TrimSpace(message) == "" {
		http.Error(w, "no assistant message is available to annotate", http.StatusConflict)
		return
	}
	if len(pending) == 0 {
		pending = in.Annotations
	}
	payload, err := json.Marshal(pending)
	if err != nil {
		http.Error(w, "could not encode annotations", http.StatusInternalServerError)
		return
	}
	prompt := "Please revise your previous response using this user annotation feedback.\n\nANNOTATED MESSAGE:\n" + message + "\n\nFEEDBACK (JSON):\n" + string(payload)
	if len(in.Attachments) > 0 {
		prompt += "\n\nATTACHED FILES (use the read tool to inspect them):\n- " + strings.Join(in.Attachments, "\n- ")
	}
	if err := a.submitToSession(prompt); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.mu.Lock()
	a.pendingAnnotations = nil
	a.mu.Unlock()
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
