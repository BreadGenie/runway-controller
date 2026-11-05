package grant

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// SetupRequest contains untrusted guest hints; job identity comes from release.
type SetupRequest struct {
	Version int          `json:"version"`
	Nonce   string       `json:"nonce"`
	Context SetupContext `json:"context"`
}

// SetupContext contains allowlisted GITHUB_* environment hints.
type SetupContext struct {
	Repository   *string `json:"repository"`
	RepositoryID *string `json:"repository_id"`
	RunID        *string `json:"run_id"`
	RunAttempt   *string `json:"run_attempt"`
	Ref          *string `json:"ref"`
	RefType      *string `json:"ref_type"`
	SHA          *string `json:"sha"`
	EventName    *string `json:"event_name"`
	BaseRef      *string `json:"base_ref"`
	HeadRef      *string `json:"head_ref"`
}

// RepositoryPolicy is read from GitHub with the host credential, never from the guest.
type RepositoryPolicy struct {
	RepositoryID  int64
	Repository    string
	DefaultBranch string
}

// DecodeSetup rejects duplicate/unknown/missing fields, nested context values,
// excessive bytes and multiple documents before returning a typed request.
func DecodeSetup(data []byte) (SetupRequest, error) {
	var result SetupRequest
	if len(data) == 0 || len(data) > 32768 {
		return result, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := setupObject(decoder, 0); err != nil {
		return result, ErrInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return result, ErrInvalid
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.Version != 2 || !canonicalUUID(result.Nonce) {
		return SetupRequest{}, ErrInvalid
	}
	return result, nil
}

func setupObject(decoder *json.Decoder, depth int) error {
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') || depth > 1 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		if depth == 0 && key == "context" {
			if setupObject(decoder, 1) != nil {
				return ErrInvalid
			}
			continue
		}
		value, err := decoder.Token()
		if err != nil {
			return ErrInvalid
		}
		if _, nested := value.(json.Delim); nested {
			return ErrInvalid
		}
		if depth == 0 {
			switch key {
			case "version":
				if _, ok := value.(json.Number); !ok {
					return ErrInvalid
				}
			case "nonce":
				if _, ok := value.(string); !ok {
					return ErrInvalid
				}
			default:
				return ErrInvalid
			}
		}
		if depth == 1 && value != nil {
			text, ok := value.(string)
			if !ok || len(text) > 8192 || len([]rune(text)) > 2048 {
				return ErrInvalid
			}
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return ErrInvalid
	}
	if depth == 0 && (len(seen) != 3 || !seen["version"] || !seen["nonce"] || !seen["context"]) {
		return ErrInvalid
	}
	if depth == 1 {
		if len(seen) != 10 {
			return ErrInvalid
		}
		for _, name := range []string{"repository", "repository_id", "run_id", "run_attempt", "ref", "ref_type", "sha", "event_name", "base_ref", "head_ref"} {
			if !seen[name] {
				return ErrInvalid
			}
		}
	}
	return nil
}

// IssueSetup selects regional caching only for verified branch push and manual
// dispatch contexts. Other event/ref shapes keep GitHub's cache; they never
// receive a broader default-branch grant. The grant reads the job's branch, then
// the default branch, and writes only the job's branch.
func IssueSetup(key ed25519.PrivateKey, audience string, a Authorization, policy RepositoryPolicy, request SetupRequest, clock Clock) (string, error) {
	c := request.Context
	if request.Version != 2 || !canonicalUUID(request.Nonce) || c.Repository == nil || policy.Repository != *c.Repository || policy.RepositoryID <= 0 || !decimalEquals(c.RepositoryID, policy.RepositoryID) || !decimalEquals(c.RunID, a.RunID) || !decimalEquals(c.RunAttempt, a.RunAttempt) || c.Ref == nil || c.RefType == nil || *c.RefType != "branch" || c.EventName == nil || (*c.EventName != "push" && *c.EventName != "workflow_dispatch") || !empty(c.BaseRef) || !empty(c.HeadRef) || c.SHA == nil || !commitSHA(*c.SHA) {
		return "", ErrInvalid
	}
	if !strings.HasPrefix(*c.Ref, "refs/heads/") || !validRef(*c.Ref) || len(*c.Ref) == len("refs/heads/") || policy.DefaultBranch == "" || !validRef("refs/heads/"+policy.DefaultBranch) {
		return "", ErrInvalid
	}
	a.RepositoryID = policy.RepositoryID
	a.ReadRefs = []string{*c.Ref}
	defaultRef := "refs/heads/" + policy.DefaultBranch
	if defaultRef != *c.Ref {
		a.ReadRefs = append(a.ReadRefs, defaultRef)
	}
	a.WriteRef = *c.Ref
	return Issue(key, audience, a, clock)
}

func decimalEquals(value *string, expected int64) bool {
	return value != nil && expected > 0 && *value == strconv.FormatInt(expected, 10)
}

func empty(value *string) bool { return value == nil || *value == "" }

func commitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
