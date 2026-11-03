package router

import (
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
)

func TestBuildPromptContainsTicketAndHistory(t *testing.T) {
	ticket := &pluginv1.Ticket{TicketRef: "ORC-12", Title: "Login kaputt", Description: "Beim Login 500."}
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	history := []HistoryEntry{
		{At: at, Author: "you@example.com", Text: "Bitte fixen."},
		{At: at.Add(time.Hour), Author: "Orchivio", Text: "Erledigt.", FromAgent: true},
	}

	task := BuildPrompt(ticket, history, "task")
	question := BuildPrompt(ticket, history, "question")

	for _, want := range []string{"# ORC-12: Login kaputt", "Beim Login 500.", "## Verlauf",
		"**you@example.com** (2026-11-03T10:00:00Z):\nBitte fixen.", "**Orchivio** (2026-11-03T11:00:00Z):\nErledigt."} {
		if !strings.Contains(task, want) {
			t.Errorf("prompt missing %q:\n%s", want, task)
		}
	}
	if strings.Index(task, "Bitte fixen.") > strings.Index(task, "Erledigt.") {
		t.Error("history out of order")
	}
	if !strings.Contains(question, "Ändere keine Dateien.") || strings.Contains(task, "Ändere keine Dateien.") {
		t.Error("question sentence wrong")
	}
	if !strings.Contains(task, "Committe nicht und pushe nicht; Orchivio übernimmt das.") || !strings.Contains(task, "ask_human") {
		t.Error("task sentences missing")
	}
	if strings.Contains(question, "ask_human") {
		t.Error("question must not mention ask_human")
	}
}
