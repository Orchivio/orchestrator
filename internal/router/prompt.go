package router

import (
	"strings"
	"time"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"github.com/orchivio/orchestrator/internal/classifier"
)

const (
	instructionQuestion = "Beantworte die Frage anhand der Repos. Ändere keine Dateien."
	instructionTask     = "Setze die Aufgabe um. Committe nicht und pushe nicht; Orchivio übernimmt das. " +
		"Nutze bei Unklarheiten das Tool `ask_human`."
)

// BuildPrompt renders ticket and conversation history into the agent prompt.
func BuildPrompt(t *pluginv1.Ticket, history []HistoryEntry, kind string) string {
	var b strings.Builder
	b.WriteString("# " + t.GetTicketRef() + ": " + t.GetTitle() + "\n\n")
	b.WriteString(t.GetDescription() + "\n\n## Verlauf\n")
	for _, h := range history {
		b.WriteString("**" + h.Author + "** (" + h.At.UTC().Format(time.RFC3339) + "):\n" + h.Text + "\n")
	}
	b.WriteString("\n")
	if kind == classifier.KindTask {
		b.WriteString(instructionTask)
	} else {
		b.WriteString(instructionQuestion)
	}
	b.WriteString("\n")
	return b.String()
}
