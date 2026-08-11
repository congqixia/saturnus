package policy

import (
	"encoding/json"
	"strings"
	"time"

	"saturnus/internal/store"
)

type Evaluation struct {
	AutoApprove bool   `json:"auto_approve"`
	RiskLevel   string `json:"risk_level"`
	Reason      string `json:"reason"`
}

func Evaluate(session store.Session, toolName string, toolInput json.RawMessage) Evaluation {
	risk := Risk(toolName, toolInput)
	if !session.AutoPass {
		return Evaluation{RiskLevel: risk, Reason: "auto-pass disabled"}
	}
	if !session.AutoPassUntil.IsZero() && time.Now().UTC().After(session.AutoPassUntil) {
		return Evaluation{RiskLevel: risk, Reason: "auto-pass expired"}
	}
	if risk != "low" {
		return Evaluation{RiskLevel: risk, Reason: "manual review required for non-low risk request"}
	}
	return Evaluation{AutoApprove: true, RiskLevel: risk, Reason: "matched low-risk auto-pass policy"}
}

func Risk(toolName string, toolInput json.RawMessage) string {
	name := strings.ToLower(toolName)
	body := strings.ToLower(string(toolInput))
	if strings.Contains(name, "read") || strings.Contains(name, "list") || strings.Contains(name, "search") {
		if hasSecretPattern(body) {
			return "high"
		}
		return "low"
	}
	if strings.Contains(name, "write") || strings.Contains(name, "edit") || strings.Contains(name, "patch") {
		if hasExternalPath(body) || hasSecretPattern(body) {
			return "high"
		}
		return "low"
	}
	if strings.Contains(name, "exec") || strings.Contains(name, "shell") || strings.Contains(name, "command") {
		if hasDangerousCommand(body) || hasSecretPattern(body) {
			return "high"
		}
		if hasAllowedCommand(body) {
			return "low"
		}
		return "medium"
	}
	if hasDangerousCommand(body) || hasSecretPattern(body) {
		return "high"
	}
	return "medium"
}

func hasAllowedCommand(s string) bool {
	allowed := []string{
		"go test",
		"go vet",
		"go run",
		"npm test",
		"npm run test",
		"npm run lint",
		"pnpm test",
		"pnpm lint",
		"yarn test",
		"yarn lint",
	}
	for _, prefix := range allowed {
		if strings.Contains(s, prefix) {
			return true
		}
	}
	return false
}

func hasDangerousCommand(s string) bool {
	dangerous := []string{
		"rm -rf",
		"sudo ",
		"chmod 777",
		"chown ",
		"git push",
		"git reset --hard",
		"kubectl apply",
		"kubectl delete",
		"terraform apply",
		"terraform destroy",
		"npm publish",
		"docker push",
		"curl ",
		"wget ",
	}
	for _, needle := range dangerous {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func hasExternalPath(s string) bool {
	external := []string{
		"\"/etc/",
		"\"/var/",
		"\"/usr/",
		"\"/bin/",
		"\"/sbin/",
		"\"/users/",
		"../",
	}
	for _, needle := range external {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func hasSecretPattern(s string) bool {
	secrets := []string{
		"api_key",
		"apikey",
		"secret",
		"token",
		"password",
		"private_key",
		".env",
	}
	for _, needle := range secrets {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
