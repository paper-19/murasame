package forgejo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Error struct {
	Method string
	Path   string
	Status int
	Body   string
}

func HasStatus(err error, status int) bool {
	var fe *Error
	return errors.As(err, &fe) && fe.Status == status
}

func (e *Error) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300]
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, b)
}

type User struct {
	Login string `json:"login"`
}

type Branch struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

type Pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   Branch `json:"head"`
	Base   Branch `json:"base"`
	User   User   `json:"user"`
}

type Comment struct {
	ID        int    `json:"id"`
	Body      string `json:"body"`
	User      User   `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type API interface {
	OpenPulls() ([]Pull, error)
	Pull(number int) (Pull, error)
	MarkMerged(number int, sha string) error
	Comments(number int) ([]Comment, error)
	Comment(number int, body string) error
	Status(sha, state, context, description, targetURL string) error
	Permission(user string) (string, error)
}

type Client struct {
	Base  string
	Owner string
	Repo  string
	Token string
	HTTP  *http.Client
}

const Page = 50

const MaxBody = 16 << 20

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func New(u, owner, repo, token string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &Client{Base: strings.TrimRight(u, "/") + "/api/v1", Owner: owner, Repo: repo, Token: token,
		HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect, Transport: tr}}
}

func (c *Client) r() string {
	return "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo)
}

func (c *Client) Call(method, path string, body, out any, query url.Values) error {
	_, err := c.call(method, path, body, out, query)
	return err
}

func (c *Client) call(method, path string, body, out any, query url.Values) (http.Header, error) {
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, &Error{method, path, 0, err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{method, path, 0, err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, &Error{method, path, 0, err.Error()}
	}
	if len(raw) > MaxBody {
		return nil, &Error{method, path, resp.StatusCode, fmt.Sprintf("response larger than %d bytes", MaxBody)}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &Error{method, path, resp.StatusCode, "redirected to " + resp.Header.Get("Location") +
			" (not followed)"}
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{method, path, resp.StatusCode, strings.ToValidUTF8(string(raw), "�")}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return resp.Header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, &Error{method, path, resp.StatusCode, "bad JSON: " + err.Error()}
	}
	return resp.Header, nil
}

const MaxPages = 1000

func pages[T any](c *Client, path string, query url.Values) ([]T, error) {
	var out []T
	size := 0
	for page := 1; page <= MaxPages; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(Page))
		var items []T
		headers, err := c.call("GET", path, nil, &items, q)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) == 0 {
			return out, nil
		}
		total := strings.TrimSpace(headers.Get("X-Total-Count"))
		n, terr := strconv.Atoi(total)
		if terr == nil && total != "" && !strings.HasPrefix(total, "-") {
			if len(out) >= n {
				return out, nil
			}
		} else if page > 1 && len(items) < size {
			return out, nil
		}
		size = max(size, len(items))
	}
	return nil, &Error{"GET", path, 0, fmt.Sprintf("more than %d pages", MaxPages)}
}

func (c *Client) OpenPulls() ([]Pull, error) {
	return pages[Pull](c, c.r()+"/pulls", url.Values{"state": {"open"}})
}

func (c *Client) Pull(number int) (Pull, error) {
	var p Pull
	err := c.Call("GET", fmt.Sprintf("%s/pulls/%d", c.r(), number), nil, &p, nil)
	return p, err
}

func (c *Client) MarkMerged(number int, sha string) error {
	return c.Call("POST", fmt.Sprintf("%s/pulls/%d/merge", c.r(), number),
		map[string]any{"Do": "manually-merged", "MergeCommitID": sha}, nil, nil)
}

func (c *Client) Comments(number int) ([]Comment, error) {
	var out []Comment
	err := c.Call("GET", fmt.Sprintf("%s/issues/%d/comments", c.r(), number), nil, &out, nil)
	return out, err
}

func (c *Client) Comment(number int, body string) error {
	return c.Call("POST", fmt.Sprintf("%s/issues/%d/comments", c.r(), number), map[string]any{"body": body}, nil, nil)
}

func (c *Client) Status(sha, state, context, description, targetURL string) error {
	if r := []rune(description); len(r) > 255 {
		description = string(r[:255])
	}
	body := map[string]any{"state": state, "context": context, "description": description}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	return c.Call("POST", c.r()+"/statuses/"+sha, body, nil, nil)
}

func (c *Client) Permission(user string) (string, error) {
	var r struct {
		Permission string `json:"permission"`
	}
	err := c.Call("GET", fmt.Sprintf("%s/collaborators/%s/permission", c.r(), url.PathEscape(user)), nil, &r, nil)
	if err != nil {
		if HasStatus(err, 404) {
			return "none", nil
		}
		return "", err
	}
	p := r.Permission
	if p == "" {
		p = "none"
	}
	return p, nil
}
