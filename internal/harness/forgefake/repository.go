package forgefake

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

func forgeRepositoryIdentity(e *Engine, c *call, match []string) response {
	forge := "github"
	if c.cli == "glab" {
		forge = "gitlab"
	}
	project, err := url.PathUnescape(match[1])
	if err != nil {
		return unhandled("invalid project path")
	}
	r := e.repo(forge, project)
	if r == nil || !strings.EqualFold(r.Host, c.flag("hostname")) {
		if forge == "github" {
			return ghHTTPNotFound()
		}
		return glabNotFound("Project")
	}
	return jsonResponse(map[string]any{"id": r.ID, "full_name": r.Project, "path_with_namespace": r.Project})
}

var fixtureSSHHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func repositorySSHConfig(e *Engine, c *call) response {
	if !c.has("config") || len(c.positional) != 1 || !fixtureSSHHost.MatchString(c.positional[0]) {
		return unhandled("ssh supports only -G for one host")
	}
	for _, option := range c.flags["option"] {
		if option != "CanonicalizeHostname=no" && option != "PermitLocalCommand=no" {
			return unhandled("unsupported ssh option %q", option)
		}
	}
	host := c.positional[0]
	if resolved := e.sshHosts[host]; resolved != "" {
		host = resolved
	}
	user := c.flag("user")
	if user == "" {
		user = "git"
	}
	return response{stdout: []byte(fmt.Sprintf("hostname %s\nuser %s\n", host, user))}
}
