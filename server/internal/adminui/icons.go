package adminui

import (
	"fmt"
	"html/template"
)

// icons is the small set the pages use, inline so nothing is fetched:
// 16-unit viewBox, 1.5-unit strokes, currentColor. Drawn to sit on the
// text baseline at 14 px.
var icons = map[string]string{
	"plus":     `<path d="M8 3v10M3 8h10"/>`,
	"chevron":  `<path d="M6 3.5 10.5 8 6 12.5"/>`,
	"back":     `<path d="M10 3.5 5.5 8 10 12.5"/>`,
	"pencil":   `<path d="M11.3 2.7a1.6 1.6 0 0 1 2.3 2.3L5.5 13H3v-2.5z"/>`,
	"trash":    `<path d="M3 4.5h10M6.5 4.5v-1h3v1M4.5 4.5l.6 8.5h5.8l.6-8.5"/>`,
	"check":    `<path d="M3 8.5 6.5 12 13 4.5"/>`,
	"x":        `<path d="M4 4l8 8M12 4l-8 8"/>`,
	"star":     `<path d="m8 2.2 1.8 3.7 4 .6-2.9 2.8.7 4L8 11.4l-3.6 1.9.7-4L2.2 6.5l4-.6z"/>`,
	"download": `<path d="M8 2.5v8M4.8 7.3 8 10.5l3.2-3.2M3 13h10"/>`,
	"upload":   `<path d="M8 10.5v-8M4.8 5.7 8 2.5l3.2 3.2M3 13h10"/>`,
	"copy":     `<rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 5.5v-2a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2"/>`,
	"key":      `<circle cx="6" cy="10" r="3"/><path d="M8.2 7.8 13.5 2.5M11 5l1.8 1.8M9.5 6.5 11 8"/>`,
	"qr":       `<rect x="2.5" y="2.5" width="4" height="4"/><rect x="9.5" y="2.5" width="4" height="4"/><rect x="2.5" y="9.5" width="4" height="4"/><path d="M9.5 9.5h1.5v1.5H9.5zM12.5 9.5h1v1M9.5 12.5v1h1M12 12h1.5v1.5"/>`,
	"warning":  `<path d="M8 2.5 14 13H2z"/><path d="M8 6.5v3M8 11.2v.3"/>`,
	"search":   `<circle cx="7" cy="7" r="4.5"/><path d="m10.5 10.5 3 3"/>`,
	"more":     `<circle cx="3" cy="8" r="1"/><circle cx="8" cy="8" r="1"/><circle cx="13" cy="8" r="1"/>`,
	"external": `<path d="M9 2.5h4.5V7M13.5 2.5 7 9M6 3.5H3.5v9h9V10"/>`,
}

// icon renders one by name; unknown names render nothing rather than break
// a page.
func icon(name string) template.HTML {
	body, ok := icons[name]
	if !ok {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<svg class="i" viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">%s</svg>`, body)) //nolint:gosec // our own markup
}
