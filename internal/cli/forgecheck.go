package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"murasame/internal/config"
	"murasame/internal/forgejo"
	"murasame/internal/gate"
	"murasame/internal/glob"
	"murasame/internal/instance"
)

type ForgeReader interface {
	Repository() (forgejo.Repository, error)
	Permission(user string) (string, error)
	Dir(path, ref string) ([]forgejo.Entry, error)
	File(path, ref string) (string, error)
	BranchProtections() ([]forgejo.BranchProtection, error)
	BranchHead(branch string) (string, error)
	Statuses(sha string) ([]forgejo.CommitStatus, error)
}

var stuckAfter = 10 * time.Minute

var newForgeReader = func(fj config.ForgejoSpec, token string) ForgeReader {
	return forgejo.New(fj.URL, fj.Owner, fj.Repo, token)
}

var workflowName = regexp.MustCompile(`(?m)^name:\s*["']?([^"'#\n]+?)["']?\s*(#.*)?$`)

type forgeCheck struct {
	w      io.Writer
	indent string
	failed bool
}

func (c *forgeCheck) say(level, msg string) {
	if level == "FAIL" {
		c.failed = true
	}
	fmt.Fprintf(c.w, "%s  forge %-4s %s\n", c.indent, level, msg)
}

func checkForge(cfg *config.Config, indent string, w io.Writer) bool {
	c := &forgeCheck{w: w, indent: indent}
	fj := cfg.Forgejo
	if fj.URL == "" || fj.Owner == "" || fj.Repo == "" {
		c.say("FAIL", "no [forgejo] url, owner and repo: nothing to check")
		return false
	}
	token := os.Getenv(fj.TokenEnv)
	if token == "" {
		c.say("FAIL", fmt.Sprintf("$%s is empty: the checks need the bot's token", fj.TokenEnv))
		return false
	}
	api := newForgeReader(fj, token)
	repo, err := api.Repository()
	if err != nil {
		c.say("FAIL", fmt.Sprintf("cannot read %s/%s with the bot's token: %v", fj.Owner, fj.Repo, err))
		return false
	}
	c.say("ok", "the bot's token reads "+repo.FullName)
	if repo.HasActions != nil && !*repo.HasActions {
		c.say("FAIL", "Actions are disabled for the repository (Settings → Units): no CI would run")
	}
	switch p, err := api.Permission(fj.BotUser); {
	case err != nil:
		c.say("WARN", fmt.Sprintf("cannot read @%s's permission: %v", fj.BotUser, err))
	case p == "write" || p == "admin" || p == "owner":
		c.say("ok", fmt.Sprintf("@%s has %s permission", fj.BotUser, p))
	default:
		c.say("FAIL", fmt.Sprintf("@%s has %s permission: it needs write to push the staging and target branches",
			fj.BotUser, p))
	}
	ref := ""
	for _, t := range cfg.Target {
		if sha, err := api.BranchHead(t); err == nil && sha != "" {
			ref = t
			break
		}
	}
	if ref == "" {
		c.say("FAIL", fmt.Sprintf("none of the target branches (%s) exists on the forge", strings.Join(cfg.Target, ", ")))
	} else {
		c.workflows(api, cfg.CI, ref)
	}
	c.runner(api, cfg.CI.Staging, fj.StatusContext)
	if rules, err := api.BranchProtections(); forgejo.HasStatus(err, 403) {
		c.say("skip", "the branch protection rules need admin access to read: whether one covers "+cfg.CI.Staging+
			" is not checked")
	} else if err != nil {
		c.say("WARN", fmt.Sprintf("cannot read the branch protection rules: %v", err))
	} else {
		for _, r := range rules {
			name := r.RuleName
			if name == "" {
				name = r.BranchName
			}
			if name == cfg.CI.Staging || glob.Match(name, cfg.CI.Staging) {
				c.say("WARN", fmt.Sprintf("the protection rule %q covers %s: the bot must be allowed to force-push and "+
					"delete it", name, cfg.CI.Staging))
			}
		}
	}
	return !c.failed
}

var (
	pushTrigger   = regexp.MustCompile(`(?m)^on:\s*(push|\[[^\]]*\bpush\b[^\]]*\])\s*$|^\s+push:\s*$`)
	branchFilters = regexp.MustCompile(`(?m)^\s+(branches|branches-ignore):`)
)

func (c *forgeCheck) workflows(api ForgeReader, ci config.CISpec, ref string) {
	var files []string
	var dirErr error
	for _, dir := range []string{".forgejo/workflows", ".gitea/workflows", ".github/workflows"} {
		entries, err := api.Dir(dir, ref)
		if err != nil {
			if !forgejo.HasStatus(err, 404) {
				dirErr = err
			}
			continue
		}
		for _, e := range entries {
			if e.Type == "file" && (strings.HasSuffix(e.Name, ".yml") || strings.HasSuffix(e.Name, ".yaml")) {
				files = append(files, e.Path)
			}
		}
		if len(files) > 0 {
			break
		}
	}
	if len(files) == 0 {
		if dirErr != nil {
			c.say("WARN", fmt.Sprintf("cannot list the workflows on %s: %v", ref, dirErr))
			return
		}
		c.say("FAIL", fmt.Sprintf("no workflow in .forgejo/workflows, .gitea/workflows or .github/workflows on %s: no CI "+
			"would run", ref))
		return
	}
	var named, everyPush, names []string
	for _, f := range files {
		text, err := api.File(f, ref)
		if err != nil {
			c.say("WARN", fmt.Sprintf("cannot read %s: %v", f, err))
			continue
		}
		name := f[strings.LastIndex(f, "/")+1:]
		if m := workflowName.FindStringSubmatch(text); m != nil {
			name = strings.TrimSpace(m[1])
		}
		names = append(names, name)
		switch {
		case strings.Contains(text, ci.Staging):
			named = append(named, f)
		case pushTrigger.MatchString(text) && !branchFilters.MatchString(text):
			everyPush = append(everyPush, f)
		}
	}
	switch {
	case len(named) > 0:
		c.say("ok", fmt.Sprintf("%s mention(s) %s", strings.Join(named, ", "), ci.Staging))
	case len(everyPush) > 0:
		c.say("ok", fmt.Sprintf("%s run(s) on every push, %s included", strings.Join(everyPush, ", "), ci.Staging))
	default:
		c.say("WARN", fmt.Sprintf("no workflow on %s mentions %s or runs on every push: make sure one runs on pushes "+
			"to it (a branch pattern may cover it)", ref, ci.Staging))
	}
	for _, ctx := range ci.Contexts {
		wf, _, ok := strings.Cut(ctx, " / ")
		if !ok || !strings.HasSuffix(ctx, " (push)") {
			c.say("WARN", fmt.Sprintf("ci.contexts: %q is not like \"<workflow> / <job> (push)\"", ctx))
			continue
		}
		if !slices.Contains(names, wf) {
			c.say("WARN", fmt.Sprintf("ci.contexts: no workflow is named %q (there are: %s)", wf, strings.Join(names, ", ")))
		}
	}
}

func (c *forgeCheck) runner(api ForgeReader, staging, own string) {
	sha, err := api.BranchHead(staging)
	switch {
	case err != nil && !forgejo.HasStatus(err, 404):
		c.say("WARN", fmt.Sprintf("cannot read %s: %v", staging, err))
		return
	case err != nil || sha == "":
		c.say("ok", "no commit on "+staging+" yet")
		return
	}
	sts, err := api.Statuses(sha)
	if err != nil {
		c.say("WARN", fmt.Sprintf("cannot read the CI statuses of %s: %v", staging, err))
		return
	}
	var newest time.Time
	latest := gate.Latest(sts, own)
	waiting := len(latest) > 0
	for _, st := range latest {
		if at, err := time.Parse(time.RFC3339, st.CreatedAt); err == nil && at.After(newest) {
			newest = at
		}
		if st.State != "pending" || !strings.HasPrefix(st.Description, "Waiting to run") &&
			!strings.HasPrefix(st.Description, "Blocked by required conditions") {
			waiting = false
		}
	}
	if waiting && !newest.IsZero() && time.Since(newest) > stuckAfter {
		c.say("WARN", fmt.Sprintf("the jobs on %s (%s) have waited %d min to start: is a runner with the workflow's "+
			"runs-on label online?", staging, sha[:min(12, len(sha))], int(time.Since(newest).Minutes())))
		return
	}
	c.say("ok", fmt.Sprintf("the jobs on %s (%s) are not stuck waiting for a runner", staging, sha[:min(12, len(sha))]))
}

func startupChecks(spec *instance.Spec, w io.Writer) {
	for _, e := range spec.Repos {
		var b strings.Builder
		checkForge(e.Cfg, "", &b)
		for _, line := range strings.Split(b.String(), "\n") {
			if strings.Contains(line, "forge WARN") || strings.Contains(line, "forge FAIL") {
				fmt.Fprintf(w, "[murasame] %s: %s\n", e.Name, strings.TrimSpace(line))
			}
		}
	}
}
