package commands

import (
	"bytes"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"golang.org/x/net/html"
)

var markdown = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList, extension.Linkify,
		extension.DefinitionList, extension.Footnote, mathBlocks{}),
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
)

var sanitizer = bluemonday.UGCPolicy()

const hiddenMark = "￼"

var lineBlocks = map[string]bool{"p": true, "li": true}

var containers = map[string]bool{"html": true, "body": true, "div": true, "ul": true, "ol": true}

var formatting = map[string]bool{"b": true, "strong": true, "i": true, "em": true, "del": true, "s": true, "strike": true,
	"ins": true, "mark": true, "sub": true, "sup": true, "span": true, "u": true, "small": true}

var blockLevel = map[string]bool{"address": true, "article": true, "aside": true, "blockquote": true, "details": true,
	"dialog": true, "dd": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hgroup": true, "hr": true, "main": true, "menu": true, "nav": true, "pre": true, "section": true,
	"summary": true, "table": true, "textarea": true, "head": true}

func Parse(body, bot string) []Line {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var out bytes.Buffer
	if err := markdown.Convert([]byte(withoutFrontMatter(body)), &out); err != nil {
		return nil
	}
	rendered := out.String()
	return inBoth(shown(sanitizer.Sanitize(rendered), bot), shown(rendered, bot))
}

func withoutFrontMatter(body string) string {
	if !strings.HasPrefix(body, "---\n") {
		return body
	}
	rest := body[len("---\n"):]
	for i := 0; i <= len(rest); {
		end := strings.IndexByte(rest[i:], '\n')
		line := rest[i:]
		if end >= 0 {
			line = rest[i : i+end]
		}
		if line == "---" || line == "..." {
			if end < 0 {
				return ""
			}
			return rest[i+end+1:]
		}
		if end < 0 {
			return body
		}
		i += end + 1
	}
	return body
}

func inBoth(lines, other []Line) []Line {
	left := map[Line]int{}
	for _, l := range other {
		left[l]++
	}
	var out []Line
	for _, l := range lines {
		if left[l] > 0 {
			left[l]--
			out = append(out, l)
		}
	}
	return out
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func concealed(n *html.Node) bool {
	if _, ok := attr(n, "hidden"); ok {
		return true
	}
	if _, ok := attr(n, "style"); ok {
		return true
	}
	class, _ := attr(n, "class")
	return strings.Contains(class, "footnotes")
}

func shown(doc, bot string) []Line {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return nil
	}
	var out []Line
	var line strings.Builder
	flush := func() {
		for _, l := range strings.Split(line.String(), "\n") {
			if rest, ok := matchHead(l, bot); ok {
				hidden := strings.Contains(rest, hiddenMark)
				if hidden {
					rest = strings.Join(strings.Fields(strings.ReplaceAll(rest, hiddenMark, " ")), " ")
				}
				rest = strings.TrimSpace(rest)
				var cmd *Line
				if !hidden {
					cmd = command(rest)
				}
				if cmd == nil {
					cmd = &Line{Unknown: true, Text: rest, Suggestion: Suggest(rest)}
				}
				out = append(out, *cmd)
			}
		}
		line.Reset()
	}
	var walk func(n *html.Node, text bool)
	walk = func(n *html.Node, text bool) {
		switch n.Type {
		case html.TextNode:
			if text {
				line.WriteString(n.Data)
			}
			return
		case html.DocumentNode:
		case html.ElementNode:
			tag := n.Data
			switch {
			case concealed(n) || !(lineBlocks[tag] || containers[tag] || formatting[tag] || tag == "br"):
				if blockLevel[tag] {
					flush()
				} else if text {
					line.WriteString(hiddenMark)
				}
				return
			case tag == "br":
				if text {
					line.WriteString("\n")
				}
				return
			case lineBlocks[tag]:
				flush()
				text = true
			case containers[tag]:
				flush()
				text = false
			}
		default:
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, text)
		}
		if n.Type == html.ElementNode && (lineBlocks[n.Data] || containers[n.Data]) {
			flush()
		}
	}
	walk(root, false)
	flush()
	return out
}
