package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleEventClearsStateWhenSessionChanges(t *testing.T) {
	a := &app{}
	a.handleEvent(frame{Event: "session_start", SessionID: "session-a"})
	a.handleEvent(frame{Event: "assistant_message", SessionID: "session-a", Text: "old response"})

	a.handleEvent(frame{Event: "session_end", SessionID: "session-a"})
	a.handleEvent(frame{Event: "session_start", SessionID: "session-b"})

	if a.latest != "" {
		t.Fatalf("latest message survived session switch: %q", a.latest)
	}
	if a.sessionID != "session-b" {
		t.Fatalf("active session = %q, want session-b", a.sessionID)
	}
}

func TestHandleEventIgnoresMessageFromAnotherSession(t *testing.T) {
	a := &app{}
	a.handleEvent(frame{Event: "session_start", SessionID: "session-b"})
	a.handleEvent(frame{Event: "assistant_message", SessionID: "session-a", Text: "stale response"})

	if a.latest != "" {
		t.Fatalf("stale message was accepted: %q", a.latest)
	}
}

func TestHandleEventAppliesSessionSnapshot(t *testing.T) {
	a := &app{}
	a.handleEvent(frame{
		Event:     "session_snapshot",
		SessionID: "session-b",
		MessageID: "entry-7",
		Snapshot:  "rewound response",
	})

	if a.latest != "rewound response" || a.sessionID != "session-b" || a.messageID != "entry-7" {
		t.Fatalf("snapshot not applied: %#v", a)
	}
}

func TestThemeStyleIsRenderedAsCSS(t *testing.T) {
	tmpl, err := template.ParseFS(uiFS, "ui/index.html")
	if err != nil {
		t.Fatal(err)
	}

	var rendered bytes.Buffer
	style := themeCSS(theme{Background: "#111111", Foreground: "#eeeeee"})
	if err := tmpl.Execute(&rendered, map[string]any{
		"Message":    "hello",
		"ThemeStyle": template.CSS(style),
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered.String(), "ZgotmplZ") {
		t.Fatal("theme CSS was escaped as unsafe template content")
	}
	if !strings.Contains(rendered.String(), style) {
		t.Fatalf("rendered page does not contain theme CSS: %q", style)
	}
}

func TestAnnotationAPIStoresPendingFeedback(t *testing.T) {
	a := &app{annotateActive: true, generation: 4}
	body := strings.NewReader(`{"generation":4,"annotation":{"text":"old phrase","comment":"replace it"}}`)
	r := httptest.NewRecorder()
	a.annotations(r, httptest.NewRequest("POST", "/api/annotations", body))
	if r.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", r.Code, http.StatusNoContent)
	}
	if len(a.pendingAnnotations) != 1 || a.pendingAnnotations[0].Comment != "replace it" {
		t.Fatalf("pending annotations = %#v", a.pendingAnnotations)
	}
}

func TestSyncMessagesAreAddedDuringAnnotationSession(t *testing.T) {
	a := &app{annotateActive: true, annotateCursor: 0}
	a.handleEvent(frame{Event: "session_start", SessionID: "session-a"})
	a.annotateActive = true
	a.handleEvent(frame{Event: "assistant_message", SessionID: "session-a", MessageID: "m1", Text: "first"})
	if !a.syncAnnotations() || len(a.annotateMessages) != 1 || a.annotateMessages[0].Text != "first" {
		t.Fatalf("synced messages = %#v", a.annotateMessages)
	}
}

func TestStateIncludesGeneration(t *testing.T) {
	a := &app{latest: "response", sessionID: "session-a", generation: 3}
	r := httptest.NewRecorder()
	a.state(r, httptest.NewRequest("GET", "/api/state", nil))

	var state map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["generation"] != float64(3) {
		t.Fatalf("generation = %v, want 3", state["generation"])
	}
}
