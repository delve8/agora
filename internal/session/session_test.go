package session

import (
	"testing"

	"github.com/delve8/agora/internal/event"
)

func TestDerivedDisplayNamePrefersLatestNonEmptyAITitle(t *testing.T) {
	values := []event.Event{
		{Kind: event.KindUser, Content: "first user request"},
		{Kind: event.KindAITitle, Content: "First title"},
		{Kind: event.KindAITitle, Content: "   "},
		{Kind: event.KindAITitle, Content: "Latest title"},
	}
	name, source := DerivedDisplayName(values)
	if name != "Latest title" || source != DisplayNameSourceAITitle {
		t.Fatalf("DerivedDisplayName() = %q, %q", name, source)
	}
}

func TestDerivedDisplayNameFallsBackToFirstUser(t *testing.T) {
	values := []event.Event{
		{Kind: event.KindAssistant, Content: "hello"},
		{Kind: event.KindUser, Content: "  inspect   the layout  "},
		{Kind: event.KindUser, Content: "later request"},
	}
	name, source := DerivedDisplayName(values)
	if name != "inspect the layout" || source != DisplayNameSourceFirstUser {
		t.Fatalf("DerivedDisplayName() = %q, %q", name, source)
	}
}

func TestResolveDerivedDisplayNamePreservesCustomName(t *testing.T) {
	value := Session{DisplayName: "My release session", DisplayNameSource: DisplayNameSourceCustom}
	name, source, apply := ResolveDerivedDisplayName(value, []event.Event{{Kind: event.KindAITitle, Content: "Generated title"}})
	if apply || name != "" || source != "" {
		t.Fatalf("custom name was replaced: %q, %q, %v", name, source, apply)
	}
}

func TestResolveDerivedDisplayNameUpgradesLegacyFirstUserName(t *testing.T) {
	values := []event.Event{
		{Kind: event.KindUser, Content: "first request"},
		{Kind: event.KindAITitle, Content: "Generated title"},
	}
	value := Session{DisplayName: "first request"}
	name, source, apply := ResolveDerivedDisplayName(value, values)
	if !apply || name != "Generated title" || source != DisplayNameSourceAITitle {
		t.Fatalf("legacy generated name was not upgraded: %q, %q, %v", name, source, apply)
	}
}
