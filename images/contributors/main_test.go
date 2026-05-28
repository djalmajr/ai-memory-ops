package main

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, raw string) payload {
	t.Helper()
	var p payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func TestAnonymousActorSkipsMutation(t *testing.T) {
	in := decode(t, `{"page":{"path":"p.md","frontmatter":null,"body":""},"ctx":{"actor":{}}}`)
	if _, ok := enrich(in); ok {
		t.Fatalf("expected ok=false for anonymous actor")
	}
}

func TestKnownActorAppendsNewContributor(t *testing.T) {
	in := decode(t, `{"page":{"path":"p.md","frontmatter":null,"body":""},"ctx":{"actor":{"agent":"claude-code","user":"djalmajr","client":"c1"}}}`)
	fm, ok := enrich(in)
	if !ok {
		t.Fatal("expected ok=true")
	}
	arr := fm["contributors"].([]any)
	if len(arr) != 1 {
		t.Fatalf("len=%d want 1", len(arr))
	}
	e := arr[0].(map[string]any)
	if e["agent"] != "claude-code" || e["client"] != "c1" || e["user"] != "djalmajr" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if writesOf(e["writes"]) != 1 {
		t.Fatalf("writes=%v want 1", e["writes"])
	}
	if e["first_seen"] != e["last_seen"] {
		t.Fatalf("first_seen != last_seen on first write")
	}
}

func TestSecondWriteSameActorIncrementsWrites(t *testing.T) {
	in1 := decode(t, `{"page":{"path":"p.md","frontmatter":null,"body":""},"ctx":{"actor":{"agent":"claude-code","client":"c1","user":"djalmajr"}}}`)
	fm1, _ := enrich(in1)
	// Round-trip through JSON so types match what would come from the engine
	// (json.Number-vs-int64 etc.).
	raw, _ := json.Marshal(map[string]any{
		"page": map[string]any{"path": "p.md", "frontmatter": fm1, "body": ""},
		"ctx":  map[string]any{"actor": map[string]any{"agent": "claude-code", "client": "c1", "user": "djalmajr"}},
	})
	in2 := decode(t, string(raw))
	fm2, _ := enrich(in2)
	arr := fm2["contributors"].([]any)
	if len(arr) != 1 {
		t.Fatalf("duplicate entry: len=%d", len(arr))
	}
	e := arr[0].(map[string]any)
	if writesOf(e["writes"]) != 2 {
		t.Fatalf("writes=%v want 2", e["writes"])
	}
}

func TestDifferentClientsCreateDistinctEntries(t *testing.T) {
	in1 := decode(t, `{"page":{"path":"p.md","frontmatter":null,"body":""},"ctx":{"actor":{"agent":"claude-code","client":"c1"}}}`)
	fm1, _ := enrich(in1)
	raw, _ := json.Marshal(map[string]any{
		"page": map[string]any{"path": "p.md", "frontmatter": fm1, "body": ""},
		"ctx":  map[string]any{"actor": map[string]any{"agent": "codex", "client": "c2"}},
	})
	in2 := decode(t, string(raw))
	fm2, _ := enrich(in2)
	arr := fm2["contributors"].([]any)
	if len(arr) != 2 {
		t.Fatalf("len=%d want 2", len(arr))
	}
	got := map[string]string{}
	for _, e := range arr {
		m := e.(map[string]any)
		got[str(m["agent"])] = str(m["client"])
	}
	if got["claude-code"] != "c1" || got["codex"] != "c2" {
		t.Fatalf("unexpected: %+v", got)
	}
}
