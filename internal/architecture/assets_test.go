package architecture

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
