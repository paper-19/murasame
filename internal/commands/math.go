package commands

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type mathBlocks struct{}

func (mathBlocks) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithBlockParsers(util.Prioritized(mathParser{}, 50)))
}

type mathParser struct{}

type mathOpen struct {
	closer []byte
	done   bool
}

var mathKey = parser.NewContextKey()

var mathOpeners = [][2][]byte{{[]byte("$$"), []byte("$$")}, {[]byte(`\[`), []byte(`\]`)}}

func (mathParser) Trigger() []byte { return []byte{'$', '\\'} }

func (mathParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 {
		return nil, parser.NoChildren
	}
	for _, o := range mathOpeners {
		if bytes.HasPrefix(line[pos:], o[0]) {
			rest := line[pos+len(o[0]):]
			node := ast.NewCodeBlock()
			openMath(pc)[node] = &mathOpen{o[1], bytes.Contains(rest, o[1])}
			reader.AdvanceToEOL()
			return node, parser.NoChildren
		}
	}
	return nil, parser.NoChildren
}

func (mathParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	m := openMath(pc)[node]
	if m == nil || m.done {
		return parser.Close
	}
	line, _ := reader.PeekLine()
	w, pos := util.IndentWidth(line, reader.LineOffset())
	reader.AdvanceToEOL()
	if w < 4 && closesMath(line[pos:], m.closer) {
		return parser.Close
	}
	return parser.Continue | parser.NoChildren
}

func closesMath(line, closer []byte) bool {
	if closer[0] == '$' {
		n := 0
		for n < len(line) && line[n] == '$' {
			n++
		}
		return n >= 2 && util.IsBlank(line[n:])
	}
	return bytes.HasPrefix(line, closer) && util.IsBlank(line[len(closer):])
}

func (mathParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {
	delete(openMath(pc), node)
}

func openMath(pc parser.Context) map[ast.Node]*mathOpen {
	if m, ok := pc.Get(mathKey).(map[ast.Node]*mathOpen); ok {
		return m
	}
	m := map[ast.Node]*mathOpen{}
	pc.Set(mathKey, m)
	return m
}

func (mathParser) CanInterruptParagraph() bool { return true }

func (mathParser) CanAcceptIndentedLine() bool { return false }
