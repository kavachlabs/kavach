package kmap

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"strings"
)

//go:embed assets/*
var assetsFS embed.FS

// RenderHTML writes a single self-contained interactive HTML document for m to w.
func RenderHTML(w io.Writer, m *Model) error {
	return renderHTML(w, m, false)
}

// RenderLiveHTML writes a live-enabled interactive HTML document for m to w.
// It includes a kavach-live meta marker so map.js subscribes to /api/events
// when served over HTTP(S).
func RenderLiveHTML(w io.Writer, m *Model) error {
	return renderHTML(w, m, true)
}

func renderHTML(w io.Writer, m *Model, live bool) error {
	rawHTML, err := assetsFS.ReadFile("assets/map.html")
	if err != nil {
		return fmt.Errorf("read embedded map.html: %w", err)
	}
	rawCSS, err := assetsFS.ReadFile("assets/map.css")
	if err != nil {
		return fmt.Errorf("read embedded map.css: %w", err)
	}
	rawJS, err := assetsFS.ReadFile("assets/map.js")
	if err != nil {
		return fmt.Errorf("read embedded map.js: %w", err)
	}
	rawDeckJS, err := assetsFS.ReadFile("assets/deck.js")
	if err != nil {
		return fmt.Errorf("read embedded deck.js: %w", err)
	}

	// Standard json.Marshal escapes <, >, &, U+2028, and U+2029 so embedded
	// fixture payloads cannot break out of the <script type="application/json"> tag.
	dataJSON, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal model: %w", err)
	}

	title := m.Title
	if title == "" {
		title = "Kavach Map"
	}

	doc := string(rawHTML)
	if live {
		doc = strings.Replace(doc, "<head>", "<head>\n  <meta name=\"kavach-live\" content=\"1\">", 1)
	}
	// One pass, so text substituted for one placeholder (fixture payloads in
	// the data, say) is never rescanned for another.
	doc = strings.NewReplacer(
		"{{KAVACH_MAP_TITLE}}", html.EscapeString(title),
		"/*KAVACH_MAP_CSS*/", string(rawCSS),
		"{{KAVACH_MAP_DATA}}", string(dataJSON),
		// The deck (Replay tab view) loads first and registers
		// window.KavachDeck, which map.js picks up.
		"/*KAVACH_MAP_DECK_JS*/", string(rawDeckJS),
		"/*KAVACH_MAP_JS*/", string(rawJS),
	).Replace(doc)

	_, err = io.WriteString(w, doc)
	return err
}
