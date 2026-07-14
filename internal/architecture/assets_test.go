package architecture

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompositionDocsTrackReleaseLocks(t *testing.T) {
	root := repoRoot(t)
	tests := []struct {
		lock string
		doc  string
		keys []string
	}{
		{lock: "engram-v0.3.0.lock.json", doc: "engram.md", keys: []string{"version", "sha256"}},
		{lock: "openclaw-2026.6.11.lock.json", doc: "openclaw.md", keys: []string{"version", "npm_sha256", "image"}},
		{lock: "hermes-agent-v2026.7.7.2.lock.json", doc: "hermes-agent.md", keys: []string{"release", "version", "commit", "source_sha256", "image"}},
	}
	for _, test := range tests {
		t.Run(test.doc, func(t *testing.T) {
			lockRaw, err := os.ReadFile(filepath.Join(root, "internal", "e2e", "testdata", test.lock))
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]any{}
			if err := json.Unmarshal(lockRaw, &values); err != nil {
				t.Fatal(err)
			}
			docRaw, err := os.ReadFile(filepath.Join(root, "docs", "compositions", test.doc))
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range test.keys {
				value, ok := values[key].(string)
				if !ok || value == "" {
					t.Fatalf("lock key %q is absent", key)
				}
				if !strings.Contains(string(docRaw), value) {
					t.Fatalf("%s does not name locked %s %q", test.doc, key, value)
				}
			}
		})
	}
}

func TestKenogramMarkIsStandaloneAccessibleSVG(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "assets", "kenogram-mark.svg")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := xml.NewDecoder(file)
	seenSVG, seenTitle, seenDescription := false, false, false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse SVG: %v", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "svg":
			seenSVG = true
			attributes := map[string]string{}
			for _, attribute := range start.Attr {
				attributes[attribute.Name.Local] = attribute.Value
			}
			if attributes["viewBox"] != "0 0 1200 680" || attributes["role"] != "img" || attributes["aria-labelledby"] != "title desc" {
				t.Fatalf("incomplete accessible SVG attributes: %#v", attributes)
			}
		case "title":
			seenTitle = true
		case "desc":
			seenDescription = true
		case "image", "script":
			t.Fatalf("standalone mark must not contain <%s>", start.Name.Local)
		case "text":
			for _, attribute := range start.Attr {
				if attribute.Name.Local == "font-family" && strings.Contains(attribute.Value, "http") {
					t.Fatal("external font reference")
				}
			}
		}
	}
	if !seenSVG || !seenTitle || !seenDescription {
		t.Fatalf("svg=%t title=%t desc=%t", seenSVG, seenTitle, seenDescription)
	}
}
