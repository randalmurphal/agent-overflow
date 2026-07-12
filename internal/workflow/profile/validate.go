package profile

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// Validate returns all independently discoverable profile findings.
func Validate(profile Profile) ValidationResult {
	var findings []Finding
	add := func(code, element, message string) {
		findings = append(findings, Finding{Code: code, Element: element, Message: message})
	}
	if profile.BaseBranch != "" && strings.TrimSpace(profile.BaseBranch) == "" {
		add("base-branch.value", "profile base_branch", "must not be blank")
	}
	validateArgvMap("check", profile.Checks, &findings)
	for name, capacity := range profile.Capacities {
		element := fmt.Sprintf("profile capacity %q", name)
		validateName("capacity", name, element, &findings)
		if capacity < 1 {
			add("capacity.value", element, "capacity must be at least 1")
		}
	}
	validateArgvMap("command", profile.Commands, &findings)
	validateReliability(profile.Reliability, &findings)
	if profile.Disposition != DispositionManual && profile.Disposition != DispositionAutoPR && profile.Disposition != DispositionAutoMerge {
		add("disposition.value", "profile disposition", "must be manual, auto-pr, or auto-merge")
	}
	for name, secret := range profile.Secrets {
		element := fmt.Sprintf("profile secret %q", name)
		validateName("secret", name, element, &findings)
		validateSecret(secret, element, &findings)
	}
	seenMCP := make(map[string]bool, len(profile.MCPServers))
	for index, server := range profile.MCPServers {
		element := fmt.Sprintf("profile mcp_servers[%d]", index)
		if strings.TrimSpace(server) == "" {
			add("mcp-server.value", element, "server name must not be empty")
		} else if seenMCP[server] {
			add("mcp-server.duplicate", element, fmt.Sprintf("server %q is duplicated", server))
		}
		seenMCP[server] = true
	}
	for index, pattern := range profile.WorktreeSetup.Copy {
		if strings.TrimSpace(pattern) == "" {
			add("worktree-setup.copy", fmt.Sprintf("profile worktree_setup.copy[%d]", index), "glob must not be empty")
		}
	}
	for index, argv := range profile.WorktreeSetup.Run {
		validateArgv(argv, fmt.Sprintf("profile worktree_setup.run[%d]", index), "worktree-setup.run", &findings)
	}
	if profile.WorktreeSetup.Timeout != "" {
		validatePositiveDuration(profile.WorktreeSetup.Timeout, "profile worktree_setup.timeout", "worktree-setup.timeout", &findings)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Element == findings[j].Element {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Element < findings[j].Element
	})
	return ValidationResult{Findings: findings}
}

func validateArgvMap(kind string, values map[string][]string, findings *[]Finding) {
	for name, argv := range values {
		element := fmt.Sprintf("profile %s %q", kind, name)
		validateName(kind, name, element, findings)
		validateArgv(argv, element, kind+".argv", findings)
	}
}

func validateName(kind, name, element string, findings *[]Finding) {
	if !namePattern.MatchString(name) {
		*findings = append(*findings, Finding{Code: kind + ".name", Element: element, Message: "name must match [a-z0-9-]+"})
	}
}

func validateArgv(argv []string, element, code string, findings *[]Finding) {
	message := "argv must contain a non-empty executable at index 0"
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		*findings = append(*findings, Finding{Code: code, Element: element, Message: message})
	}
}

func validateReliability(reliability ReliabilityDefaults, findings *[]Finding) {
	if reliability.Watchdog != "" {
		validatePositiveDuration(reliability.Watchdog, "profile reliability.watchdog", "reliability.watchdog", findings)
	}
	if reliability.Backoff != nil {
		if len(reliability.Backoff) == 0 {
			*findings = append(*findings, Finding{Code: "reliability.backoff", Element: "profile reliability.backoff", Message: "must contain at least one retry delay"})
		}
		for index, delay := range reliability.Backoff {
			validatePositiveDuration(delay, fmt.Sprintf("profile reliability.backoff[%d]", index), "reliability.backoff", findings)
		}
	}
	if reliability.PerItemBudget == nil {
		return
	}
	*findings = append(*findings, validateBudget(reliability.PerItemBudget, "profile reliability.per_item_budget")...)
}

// ValidateBudget validates an optional item-level budget using the same
// exactly-one-field contract as profile reliability defaults.
func ValidateBudget(budget *Budget) ValidationResult {
	findings := validateBudget(budget, "work item budget")
	return ValidationResult{Findings: findings}
}

func validateBudget(budget *Budget, element string) []Finding {
	if budget == nil {
		return nil
	}
	var findings []Finding
	set := 0
	if budget.Tokens != nil {
		set++
		if *budget.Tokens < 1 {
			findings = append(findings, Finding{Code: "reliability.budget-tokens", Element: element + ".tokens", Message: "must be at least 1"})
		}
	}
	if budget.USD != nil {
		set++
		if *budget.USD <= 0 || math.IsNaN(*budget.USD) || math.IsInf(*budget.USD, 0) {
			findings = append(findings, Finding{Code: "reliability.budget-usd", Element: element + ".usd", Message: "must be a finite value greater than 0"})
		}
	}
	if budget.WallClock != nil {
		set++
		validatePositiveDuration(*budget.WallClock, element+".wall_clock", "reliability.budget-wall-clock", &findings)
	}
	if set != 1 {
		findings = append(findings, Finding{Code: "reliability.budget", Element: element, Message: "exactly one of tokens, usd, or wall_clock must be set"})
	}
	return findings
}

func validatePositiveDuration(value Duration, element, code string, findings *[]Finding) {
	duration, err := time.ParseDuration(string(value))
	if err != nil {
		*findings = append(*findings, Finding{Code: code, Element: element, Message: "must be a time.ParseDuration-compatible string"})
	} else if duration <= 0 {
		*findings = append(*findings, Finding{Code: code, Element: element, Message: "must be greater than 0"})
	}
}

func validateSecret(secret Secret, element string, findings *[]Finding) {
	add := func(code, message string) {
		*findings = append(*findings, Finding{Code: code, Element: element, Message: message})
	}
	switch secret.Source {
	case "env":
		if strings.TrimSpace(secret.Env) == "" {
			add("secret.env", "env source requires a non-empty env field")
		}
		if secret.Path != "" {
			add("secret.path", "env source must not set path")
		}
	case "file":
		if strings.TrimSpace(secret.Path) == "" {
			add("secret.path", "file source requires a non-empty path field")
		}
		if secret.Env != "" {
			add("secret.env", "file source must not set env")
		}
	default:
		add("secret.source", "source must be env or file")
	}
}
