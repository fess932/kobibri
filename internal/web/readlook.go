package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type readLook struct {
	Theme string
	Font  string
	Size  int
}

const (
	readLookCookie = "reader"

	readSizeMin     = 14
	readSizeMax     = 30
	readSizeStep    = 2
	readSizeDefault = 19
)

var readThemes = []string{"auto", "light", "sepia", "dark"}

func defaultReadLook() readLook {
	return readLook{Theme: "auto", Font: "serif", Size: readSizeDefault}
}

func (l readLook) with(values url.Values) readLook {
	if v := values.Get("theme"); v != "" {
		for _, known := range readThemes {
			if v == known {
				l.Theme = v
			}
		}
	}
	switch v := values.Get("font"); v {
	case "serif", "sans":
		l.Font = v
	}
	if n, err := strconv.Atoi(values.Get("size")); err == nil {
		l.Size = min(max(n, readSizeMin), readSizeMax)
	}
	return l
}

func (l readLook) query() string {
	return url.Values{
		"theme": {l.Theme}, "font": {l.Font}, "size": {strconv.Itoa(l.Size)},
	}.Encode()
}

func readLookOf(r *http.Request) (look readLook, chosen bool) {
	look = defaultReadLook()
	if c, err := r.Cookie(readLookCookie); err == nil {
		if saved, err := url.ParseQuery(c.Value); err == nil {
			look = look.with(saved)
		}
	}
	q := r.URL.Query()
	chosen = q.Has("theme") || q.Has("font") || q.Has("size")
	return look.with(q), chosen
}

func rememberReadLook(w http.ResponseWriter, look readLook) {
	http.SetCookie(w, &http.Cookie{
		Name: readLookCookie, Value: look.query(), Path: "/",
		Expires: time.Now().AddDate(1, 0, 0), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

type readPalette struct{ paper, ink, link, rule string }

var (
	readLight = readPalette{"#fbfaf7", "#1b1c20", "#2e4b9b", "#dcdde3"}
	readSepia = readPalette{"#f4ecd8", "#3a2f20", "#7a4a1c", "#d9cdb0"}
	readDark  = readPalette{"#15171c", "#d5d7de", "#8aa6ee", "#2c303a"}
)

func (p readPalette) vars() string {
	return fmt.Sprintf("--paper:%s;--ink:%s;--link:%s;--rule:%s", p.paper, p.ink, p.link, p.rule)
}

func (l readLook) css() string {
	var palette string
	switch l.Theme {
	case "light":
		palette = ":root{" + readLight.vars() + "}"
	case "sepia":
		palette = ":root{" + readSepia.vars() + "}"
	case "dark":
		palette = ":root{" + readDark.vars() + "}"
	default:
		palette = ":root{" + readLight.vars() + "}" +
			"@media (prefers-color-scheme:dark){:root{" + readDark.vars() + "}}"
	}

	font := `ui-serif,"Iowan Old Style","Charter","PT Serif",Georgia,"Times New Roman",serif`
	if l.Font == "sans" {
		font = `-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif`
	}

	return palette + fmt.Sprintf(`
html{background:var(--paper) !important}
body{background:var(--paper) !important;color:var(--ink) !important;
  font-family:%s !important;font-size:%dpx !important;line-height:1.65 !important;
  max-width:40em !important;margin:0 auto !important;padding:28px 22px 96px !important;
  overflow-wrap:break-word;text-rendering:optimizeLegibility;-webkit-text-size-adjust:100%%}
body *{color:inherit !important;background-color:transparent !important;
  font-family:inherit !important;max-width:100%% !important;box-sizing:border-box}
p,li,blockquote,dd,td{font-size:1em !important;line-height:inherit !important}
p{margin:0 0 .85em !important}
h1,h2,h3,h4{line-height:1.25 !important;margin:1.6em 0 .7em !important}
img,svg,video,picture{max-width:100%% !important;height:auto !important}
p>img:only-child,div>img:only-child,figure img{display:block;margin:1.2em auto !important}
p:has(>img){text-indent:0 !important}
a{color:var(--link) !important}
hr{border:0 !important;border-top:1px solid var(--rule) !important;margin:2em 0 !important}
table{border-collapse:collapse}
pre,code{font-family:ui-monospace,Menlo,Consolas,monospace !important;white-space:pre-wrap}
`, font, l.Size)
}

func (l readLook) dress(doc []byte) []byte {
	style := []byte(`<meta name="viewport" content="width=device-width, initial-scale=1"><style>` +
		l.css() + `</style>`)

	lower := bytes.ToLower(doc)
	if i := bytes.Index(lower, []byte("</head>")); i >= 0 {
		return bytes.Join([][]byte{doc[:i], style, doc[i:]}, nil)
	}
	if i := bytes.Index(lower, []byte("<body")); i >= 0 {
		return bytes.Join([][]byte{doc[:i], style, doc[i:]}, nil)
	}
	return append(style, doc...)
}

type readControl struct {
	Label  string
	Href   string
	Active bool
	Off    bool
}

func readControls(lang Lang, base string, at int, look readLook) (sizes, fonts, themes []readControl) {
	href := func(l readLook) string {
		return base + "?at=" + strconv.Itoa(at) + "&" + l.query()
	}
	withSize := func(n int) readLook { l := look; l.Size = n; return l }
	withFont := func(f string) readLook { l := look; l.Font = f; return l }
	withTheme := func(t string) readLook { l := look; l.Theme = t; return l }

	sizes = []readControl{
		{Label: "A−", Href: href(withSize(look.Size - readSizeStep)), Off: look.Size <= readSizeMin},
		{Label: "A+", Href: href(withSize(look.Size + readSizeStep)), Off: look.Size >= readSizeMax},
	}
	for _, f := range []string{"serif", "sans"} {
		fonts = append(fonts, readControl{
			Label: T(lang, "read.font."+f), Href: href(withFont(f)), Active: look.Font == f,
		})
	}
	for _, t := range readThemes {
		themes = append(themes, readControl{
			Label: T(lang, "read.theme."+t), Href: href(withTheme(t)), Active: look.Theme == t,
		})
	}
	return sizes, fonts, themes
}

func isHTML(contentType string) bool { return strings.HasPrefix(contentType, "text/html") }
