package commands

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var Simple = []string{"approve", "unapprove", "retry", "cancel", "status", "ping"}

var Names = append(append([]string{}, Simple...), "priority", "rollup")

var Privileged = map[string]bool{"approve": true, "unapprove": true, "priority": true, "rollup": true, "retry": true,
	"cancel": true}

const PriorityRange = 9999

type Line struct {
	Name       string
	Arg        string
	Unknown    bool
	Text       string
	Suggestion string
}

var prioRE = regexp.MustCompile(`^(?:0|-?[1-9]\d{0,3})$`)

var shaRE = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

func FullSha(s string) bool { return (len(s) == 40 || len(s) == 64) && shaRE.MatchString(s) }

func command(rest string) *Line {
	words := strings.Fields(rest)
	if len(words) == 0 {
		return nil
	}
	w0 := strings.ToLower(words[0])
	if len(words) == 1 && slices.Index(Simple, w0) >= 0 {
		return &Line{Name: w0}
	}
	if w0 == "approve" && len(words) == 2 && shaRE.MatchString(words[1]) {
		return &Line{Name: "approve", Arg: strings.ToLower(words[1])}
	}
	if w0 == "priority" && len(words) == 2 && prioRE.MatchString(words[1]) {
		n, _ := strconv.Atoi(words[1])
		return &Line{Name: "priority", Arg: strconv.Itoa(n)}
	}
	if w0 == "rollup" && len(words) == 1 {
		return &Line{Name: "rollup", Arg: "maybe"}
	}
	if w0 == "rollup" && len(words) == 2 && strings.ToLower(words[1]) == "never" {
		return &Line{Name: "rollup", Arg: "never"}
	}
	return nil
}

func Distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ca := range ra {
		cur := []int{i + 1}
		for j, cb := range rb {
			c := prev[j]
			if ca != cb {
				c++
			}
			cur = append(cur, min(prev[j+1]+1, cur[j]+1, c))
		}
		prev = cur
	}
	return prev[len(prev)-1]
}

var (
	reApprove   = regexp.MustCompile(`^(?:r\s*(?:\+|=)|\+1\b)`)
	reUnapprove = regexp.MustCompile(`^(?:r\s*-|-1\b)`)
	rePrio      = regexp.MustCompile(`^(?:p|prio|priority)\b\s*[=:]?\s*\+?(-?\d+)`)
	reNever     = regexp.MustCompile(`^rollup\s*(-|=\s*never|:\s*never|never\b|no\b|false\b)`)
	reRollup    = regexp.MustCompile(`^rollup\b`)
	reNonAlpha  = regexp.MustCompile(`[^a-z]`)
	reNum       = regexp.MustCompile(`-?\d+`)
)

var synonyms = map[string]string{
	"merge": "approve", "lgtm": "approve", "ship": "approve", "accept": "approve", "land": "approve", "queue": "approve",
	"help": "status", "info": "status", "state": "status", "show": "status",
	"dequeue": "unapprove", "revoke": "unapprove", "withdraw": "unapprove",
	"stop": "cancel", "abort": "cancel", "hold": "cancel",
	"rerun": "retry", "requeue": "retry", "resume": "retry", "restart": "retry",
}

func Suggest(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return "status"
	}
	if reApprove.MatchString(t) {
		return "approve"
	}
	if reUnapprove.MatchString(t) {
		return "unapprove"
	}
	if m := rePrio.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > PriorityRange {
			n = PriorityRange
			if strings.HasPrefix(m[1], "-") {
				n = -PriorityRange
			}
		}
		n = max(-PriorityRange, min(PriorityRange, n))
		return "priority " + strconv.Itoa(n)
	}
	if reNever.MatchString(t) {
		return "rollup never"
	}
	if reRollup.MatchString(t) {
		return "rollup"
	}
	word := reNonAlpha.ReplaceAllString(strings.Fields(t)[0], "")
	if s, ok := synonyms[word]; ok {
		return s
	}
	if word == "" {
		word = t
	}
	best := Names[0]
	bestD := Distance(word, best)
	for _, n := range Names[1:] {
		if d := Distance(word, n); d < bestD {
			best, bestD = n, d
		}
	}
	if best == "priority" {
		num := "1"
		if m := reNum.FindString(t); m != "" {
			num = m
		}
		return "priority " + num
	}
	return best
}

func matchHead(line, bot string) (string, bool) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || line[i] != '@' || len(line) < i+1+len(bot) || !asciiFold(line[i+1:i+1+len(bot)], bot) {
		return "", false
	}
	rest := line[i+1+len(bot):]
	if rest == "" {
		return "", true
	}
	if rest[0] == ':' || rest[0] == ',' {
		return rest[1:], true
	}
	if r, _ := utf8.DecodeRuneInString(rest); unicode.IsSpace(r) {
		return rest, true
	}
	return "", false
}

func asciiFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func ApproveLine(bot, head string) string { return "@" + bot + " approve " + head }

func UnknownReply(u Line, bot, head string) string {
	shown := strings.ReplaceAll(u.Text, "`", "'")
	if r := []rune(shown); len(r) > 60 {
		shown = string(r[:57]) + "..."
	}
	what := "`" + shown + "`"
	if shown == "" {
		what = "(nothing after `@" + bot + "`)"
	}
	suggestion := "@" + bot + " " + u.Suggestion
	if u.Suggestion == "approve" && head != "" {
		suggestion = ApproveLine(bot, head)
	}
	return "Unknown command " + what + ". Did you mean `" + suggestion + "`?"
}
