package architecture

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
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
	labelReferences := map[string]bool{}
	labelElements := map[string]bool{}
	labelText := map[string]string{}
	titleID, descriptionID, currentLabelID := "", "", ""
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse SVG: %v", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			attributes := map[string]string{}
			for _, attribute := range token.Attr {
				attributes[attribute.Name.Local] = attribute.Value
			}
			switch token.Name.Local {
			case "svg":
				seenSVG = true
				viewBox := strings.Fields(attributes["viewBox"])
				if len(viewBox) != 4 {
					t.Fatalf("invalid SVG viewBox %q", attributes["viewBox"])
				}
				viewBoxNumbers := make([]float64, len(viewBox))
				for index, value := range viewBox {
					viewBoxNumbers[index], err = strconv.ParseFloat(value, 64)
					if err != nil {
						t.Fatalf("invalid SVG viewBox %q: %v", attributes["viewBox"], err)
					}
				}
				if math.IsNaN(viewBoxNumbers[2]) || math.IsNaN(viewBoxNumbers[3]) ||
					math.IsInf(viewBoxNumbers[2], 0) || math.IsInf(viewBoxNumbers[3], 0) ||
					viewBoxNumbers[2] <= 0 || viewBoxNumbers[3] <= 0 {
					t.Fatalf("SVG viewBox must have positive dimensions: %q", attributes["viewBox"])
				}
				if attributes["role"] != "img" {
					t.Fatalf("SVG role must be img: %#v", attributes)
				}
				for _, identifier := range strings.Fields(attributes["aria-labelledby"]) {
					labelReferences[identifier] = true
				}
				if len(labelReferences) == 0 {
					t.Fatal("SVG must reference accessible labels with aria-labelledby")
				}
			case "title", "desc":
				identifier := attributes["id"]
				if identifier == "" {
					t.Fatalf("SVG <%s> must have an id", token.Name.Local)
				}
				labelElements[identifier] = true
				currentLabelID = identifier
				if token.Name.Local == "title" {
					seenTitle = true
					titleID = identifier
				} else {
					seenDescription = true
					descriptionID = identifier
				}
			case "image", "script":
				t.Fatalf("standalone mark must not contain <%s>", token.Name.Local)
			case "text":
				if strings.Contains(attributes["font-family"], "http") {
					t.Fatal("external font reference")
				}
			}
		case xml.CharData:
			if currentLabelID != "" {
				labelText[currentLabelID] += string(token)
			}
		case xml.EndElement:
			if token.Name.Local == "title" || token.Name.Local == "desc" {
				if strings.TrimSpace(labelText[currentLabelID]) == "" {
					t.Fatalf("SVG <%s> must not be empty", token.Name.Local)
				}
				currentLabelID = ""
			}
		}
	}
	if !seenSVG || !seenTitle || !seenDescription {
		t.Fatalf("svg=%t title=%t desc=%t", seenSVG, seenTitle, seenDescription)
	}
	if !labelReferences[titleID] || !labelReferences[descriptionID] {
		t.Fatalf("aria-labelledby does not reference title %q and description %q", titleID, descriptionID)
	}
	for identifier := range labelReferences {
		if !labelElements[identifier] {
			t.Fatalf("aria-labelledby references missing element %q", identifier)
		}
	}
}
