package main

import (
	"encoding/json"
	"net/http/httptest"
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
