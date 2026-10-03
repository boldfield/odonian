package evaluation

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

const redactedMarker = "[redacted]"

// redactor scrubs resolved credentials from everything the host records. It
// matches each secret literally and in its JSON-escaped forms, longest first,
// so a secret that contains another secret is still removed whole.
type redactor struct {
	needles []string
	longest int
}

func newRedactor(secrets []string) *redactor {
	seen := map[string]bool{}
	r := &redactor{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		r.needles = append(r.needles, s)
	}
	for _, s := range secrets {
		add(s)
		add(jsonEscaped(s, true))
		add(jsonEscaped(s, false))
	}
	sort.Slice(r.needles, func(i, j int) bool {
		if len(r.needles[i]) != len(r.needles[j]) {
			return len(r.needles[i]) > len(r.needles[j])
		}
		return r.needles[i] < r.needles[j]
	})
	if len(r.needles) > 0 {
		r.longest = len(r.needles[0])
	}
	return r
}

func jsonEscaped(s string, escapeHTML bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	return out[1 : len(out)-1]
}

func (r *redactor) str(s string) string {
	for _, n := range r.needles {
		s = strings.ReplaceAll(s, n, redactedMarker)
	}
	return s
}

func (r *redactor) bytes(b []byte) []byte {
	for _, n := range r.needles {
		b = bytes.ReplaceAll(b, []byte(n), []byte(redactedMarker))
	}
	return b
}

// dropPartialTail removes a trailing fragment that is a proper prefix of a
// secret, which is what remains when capture cut a secret in half.
func (r *redactor) dropPartialTail(s string) string {
	for k := min(len(s), r.longest-1); k > 0; k-- {
		tail := s[len(s)-k:]
		for _, n := range r.needles {
			if len(n) > k && strings.HasPrefix(n, tail) {
				return s[:len(s)-k]
			}
		}
	}
	return s
}

func (r *redactor) strs(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.str(s)
	}
	return out
}

func (r *redactor) settings(s Settings) Settings {
	if s.Values == nil {
		return s
	}
	vals := make(map[string]string, len(s.Values))
	for k, v := range s.Values {
		vals[r.str(k)] = r.str(v)
	}
	return Settings{Known: s.Known, Values: vals}
}

// response redacts every adapter-supplied string in a decoded response. It
// returns a copy; the input is not modified.
func (r *redactor) response(in CandidateResponse) CandidateResponse {
	if len(r.needles) == 0 {
		return in
	}
	out := in
	out.RunID = r.str(in.RunID)
	out.ErrorMessage = truncate(r.str(in.ErrorMessage))
	out.MissingCapabilities = r.strs(in.MissingCapabilities)
	if in.Findings != nil {
		out.Findings = make([]Finding, len(in.Findings))
		for i, f := range in.Findings {
			out.Findings[i] = Finding{
				ID: r.str(f.ID), Severity: f.Severity, Claim: r.str(f.Claim),
				Summary: r.str(f.Summary), Evidence: r.str(f.Evidence),
			}
		}
	}
	if in.Usage != nil {
		out.Usage = make(map[string]float64, len(in.Usage))
		for k, v := range in.Usage {
			out.Usage[r.str(k)] = v
		}
	}
	id := in.Identity
	id.AdapterName, id.AdapterVersion = r.str(id.AdapterName), r.str(id.AdapterVersion)
	id.ModelID, id.ModelRevision = r.str(id.ModelID), r.str(id.ModelRevision)
	id.RuntimeName, id.RuntimeVersion = r.str(id.RuntimeName), r.str(id.RuntimeVersion)
	id.PromptVersion, id.AccountPool = r.str(id.PromptVersion), r.str(id.AccountPool)
	id.ReasoningSettings = r.settings(id.ReasoningSettings)
	id.GenerationSettings = r.settings(id.GenerationSettings)
	id.Tools = NameSet{Known: id.Tools.Known, Names: r.strs(id.Tools.Names)}
	id.Observers = NameSet{Known: id.Observers.Known, Names: r.strs(id.Observers.Names)}
	out.Identity = id
	return out
}
