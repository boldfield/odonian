package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/boldfield/odonian/internal/tuiclient"
)

// Evaluation verbs drive the isolated reviewer-comparison lifecycle. Each prints
// the server's JSON response as one compact line on stdout. A refusal exits
// through admissionError: 409 (paused, exhausted, stale, finalized, ...) exits
// exitConflict and 429 (pool denial) exits exitScheduling, with the stable error
// code in the message on stderr.

var evaluationVerbs = map[string]bool{
	"evaluation-pool-set":            true,
	"evaluation-pool-get":            true,
	"evaluation-create-campaign":     true,
	"evaluation-get-campaign":        true,
	"evaluation-get-campaign-status": true,
	"evaluation-pause-campaign":      true,
	"evaluation-create-candidate":    true,
	"evaluation-list-candidates":     true,
	"evaluation-get-sample":          true,
	"evaluation-claim-job":           true,
	"evaluation-renew-attempt":       true,
	"evaluation-finalize-attempt":    true,
	"evaluation-record-disposition":  true,
	"evaluation-list-dispositions":   true,
	"evaluation-get-report":          true,
}

func executeEvaluation(ctx context.Context, verb, baseURL, token string, args []string, out io.Writer) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var method, path string
	var body interface{}
	var err error
	switch verb {
	case "evaluation-pool-set":
		method, path, body, err = evaluationPoolSet(fs, args)
	case "evaluation-pool-get":
		id := fs.String("id", "", "pool ID")
		if err = parseEvalFlags(fs, args, requireFlags(id, "--id")); err == nil {
			method, path = http.MethodGet, "/evaluation/pools/"+url.PathEscape(*id)
		}
	case "evaluation-create-campaign":
		method, path, body, err = evaluationCreateCampaign(fs, args)
	case "evaluation-get-campaign":
		id := fs.String("id", "", "campaign ID")
		if err = parseEvalFlags(fs, args, requireFlags(id, "--id")); err == nil {
			method, path = http.MethodGet, "/evaluation/campaigns/"+url.PathEscape(*id)
		}
	case "evaluation-get-campaign-status":
		id := fs.String("id", "", "campaign ID")
		if err = parseEvalFlags(fs, args, requireFlags(id, "--id")); err == nil {
			method, path = http.MethodGet, "/evaluation/campaigns/"+url.PathEscape(*id)+"/status"
		}
	case "evaluation-pause-campaign":
		id := fs.String("id", "", "campaign ID")
		if err = parseEvalFlags(fs, args, requireFlags(id, "--id")); err == nil {
			method, path = http.MethodPost, "/evaluation/campaigns/"+url.PathEscape(*id)+"/pause"
		}
	case "evaluation-create-candidate":
		method, path, body, err = evaluationCreateCandidate(fs, args)
	case "evaluation-list-candidates":
		campaign := fs.String("campaign", "", "campaign ID")
		if err = parseEvalFlags(fs, args, requireFlags(campaign, "--campaign")); err == nil {
			method, path = http.MethodGet, "/evaluation/campaigns/"+url.PathEscape(*campaign)+"/candidates"
		}
	case "evaluation-get-sample":
		campaign := fs.String("campaign", "", "campaign ID")
		sample := fs.String("sample", "", "sample ID")
		if err = parseEvalFlags(fs, args, requireFlags(campaign, "--campaign", sample, "--sample")); err == nil {
			method, path = http.MethodGet, "/evaluation/campaigns/"+url.PathEscape(*campaign)+"/samples/"+url.PathEscape(*sample)
		}
	case "evaluation-claim-job":
		method, path, body, err = evaluationClaimJob(fs, args)
	case "evaluation-renew-attempt":
		method, path, body, err = evaluationRenewAttempt(fs, args)
	case "evaluation-finalize-attempt":
		method, path, body, err = evaluationFinalizeAttempt(fs, args)
	case "evaluation-record-disposition":
		method, path, body, err = evaluationRecordDisposition(fs, args)
	case "evaluation-list-dispositions":
		campaign := fs.String("campaign", "", "campaign ID")
		sample := fs.String("sample", "", "sample ID")
		if err = parseEvalFlags(fs, args, requireFlags(campaign, "--campaign", sample, "--sample")); err == nil {
			method, path = http.MethodGet, "/evaluation/campaigns/"+url.PathEscape(*campaign)+"/samples/"+url.PathEscape(*sample)+"/dispositions"
		}
	case "evaluation-get-report":
		campaign := fs.String("campaign", "", "campaign ID")
		if err = parseEvalFlags(fs, args, requireFlags(campaign, "--campaign")); err == nil {
			method, path = http.MethodGet, "/evaluation/campaigns/"+url.PathEscape(*campaign)+"/report"
		}
	default:
		return fmt.Errorf("unknown command %q", verb)
	}
	if err != nil {
		return err
	}

	client := tuiclient.NewHTTPClient(baseURL, token)
	raw, err := client.Evaluation(ctx, method, path, body)
	if err != nil {
		if mapped := admissionError(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("%s failed: %w", verb, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return fmt.Errorf("failed to format JSON: %w", err)
	}
	fmt.Fprintln(out, compact.String())
	return nil
}

// requireFlags builds a check that each named string flag is non-empty. Its
// arguments alternate flag pointer and display name.
func requireFlags(pairs ...interface{}) func() error {
	return func() error {
		var missing []string
		for i := 0; i+1 < len(pairs); i += 2 {
			if *(pairs[i].(*string)) == "" {
				missing = append(missing, pairs[i+1].(string))
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s required", strings.Join(missing, ", "))
		}
		return nil
	}
}

func parseEvalFlags(fs *flag.FlagSet, args []string, check func() error) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return check()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readJSONInput takes inline JSON or, when file is set, the file's contents.
func readJSONInput(inline, file, what string) (json.RawMessage, error) {
	if inline != "" && file != "" {
		return nil, fmt.Errorf("give --%s or --%s-file, not both", what, what)
	}
	data := []byte(inline)
	if file != "" {
		var err error
		if data, err = os.ReadFile(file); err != nil {
			return nil, fmt.Errorf("failed to read --%s-file: %w", what, err)
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("--%s is not valid JSON", what)
	}
	return json.RawMessage(data), nil
}

func evaluationPoolSet(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	id := fs.String("id", "", "pool ID")
	concurrencyOnly := fs.Bool("concurrency-only", false, "compute pool bounded by concurrency alone (no start rate)")
	rate := fs.Float64("start-rate", 0, "sustained starts per second (rate pools)")
	burst := fs.Int("burst", 0, "burst capacity (rate pools)")
	limit := fs.Int("concurrent-limit", 0, "maximum concurrent attempts")
	if err := parseEvalFlags(fs, args, requireFlags(id, "--id")); err != nil {
		return "", "", nil, err
	}
	if *limit < 1 {
		return "", "", nil, fmt.Errorf("--concurrent-limit must be at least 1")
	}
	body := map[string]interface{}{"concurrency_only": *concurrencyOnly, "concurrent_limit": *limit}
	if *concurrencyOnly {
		if *rate != 0 || *burst != 0 {
			return "", "", nil, fmt.Errorf("--concurrency-only cannot be combined with --start-rate or --burst")
		}
	} else {
		if *rate <= 0 || *burst < 1 {
			return "", "", nil, fmt.Errorf("a rate pool needs --start-rate and --burst (or use --concurrency-only)")
		}
		body["start_rate"], body["burst_capacity"] = *rate, *burst
	}
	return http.MethodPut, "/evaluation/pools/" + url.PathEscape(*id), body, nil
}

func evaluationCreateCampaign(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	id := fs.String("id", "", "campaign ID")
	name := fs.String("name", "", "campaign name")
	description := fs.String("description", "", "campaign description (optional)")
	projects := fs.String("projects", "", "comma-separated allowed project IDs")
	models := fs.String("models", "", "comma-separated allowed candidate model IDs")
	cohort := fs.String("cohort", "", "cohort manifest")
	cap := fs.Int("cap", 0, "campaign attempt cap (finite, at least 1)")
	if err := parseEvalFlags(fs, args, requireFlags(id, "--id", name, "--name", projects, "--projects", models, "--models", cohort, "--cohort")); err != nil {
		return "", "", nil, err
	}
	if *cap < 1 {
		return "", "", nil, fmt.Errorf("--cap must be at least 1")
	}
	body := map[string]interface{}{
		"id": *id, "name": *name, "allowed_project_ids": splitList(*projects),
		"allowed_model_ids": splitList(*models), "cohort_manifest": *cohort, "attempt_cap": *cap,
	}
	if *description != "" {
		body["description"] = *description
	}
	return http.MethodPost, "/evaluation/campaigns", body, nil
}

func evaluationCreateCandidate(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	campaign := fs.String("campaign", "", "campaign ID")
	id := fs.String("id", "", "candidate version ID")
	cap := fs.Int("cap", 0, "per-candidate attempt cap (finite, at least 1)")
	identity := fs.String("identity", "", "candidate identity JSON")
	identityFile := fs.String("identity-file", "", "file holding the candidate identity JSON")
	if err := parseEvalFlags(fs, args, requireFlags(campaign, "--campaign", id, "--id")); err != nil {
		return "", "", nil, err
	}
	if *cap < 1 {
		return "", "", nil, fmt.Errorf("--cap must be at least 1")
	}
	raw, err := readJSONInput(*identity, *identityFile, "identity")
	if err != nil {
		return "", "", nil, err
	}
	if raw == nil {
		return "", "", nil, fmt.Errorf("--identity or --identity-file required")
	}
	body := map[string]interface{}{"id": *id, "per_candidate_cap": *cap, "identity": raw}
	return http.MethodPost, "/evaluation/campaigns/" + url.PathEscape(*campaign) + "/candidates", body, nil
}

func evaluationClaimJob(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	sample := fs.String("sample", "", "sample ID")
	candidate := fs.String("candidate", "", "candidate version ID")
	requestID := fs.String("request-id", "", "stable request ID; a repeat returns the original attempt")
	ttl := fs.Int64("ttl-ms", 0, "lease TTL in milliseconds (1-3600000)")
	if err := parseEvalFlags(fs, args, requireFlags(sample, "--sample", candidate, "--candidate", requestID, "--request-id")); err != nil {
		return "", "", nil, err
	}
	if *ttl < 1 {
		return "", "", nil, fmt.Errorf("--ttl-ms must be at least 1")
	}
	body := map[string]interface{}{
		"sample_id": *sample, "candidate_id": *candidate, "request_id": *requestID, "lease_ttl_ms": *ttl,
	}
	return http.MethodPost, "/evaluation/jobs/claim", body, nil
}

func evaluationRenewAttempt(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	job := fs.String("job", "", "job ID")
	attempt := fs.String("attempt", "", "attempt ID")
	ttl := fs.Int64("ttl-ms", 0, "new lease TTL in milliseconds from now (1-3600000)")
	if err := parseEvalFlags(fs, args, requireFlags(job, "--job", attempt, "--attempt")); err != nil {
		return "", "", nil, err
	}
	if *ttl < 1 {
		return "", "", nil, fmt.Errorf("--ttl-ms must be at least 1")
	}
	return http.MethodPost, evaluationAttemptPath(*job, *attempt, "renew"), map[string]interface{}{"lease_ttl_ms": *ttl}, nil
}

func evaluationAttemptPath(job, attempt, action string) string {
	return "/evaluation/jobs/" + url.PathEscape(job) + "/attempts/" + url.PathEscape(attempt) + "/" + action
}

func evaluationRecordDisposition(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	campaign := fs.String("campaign", "", "campaign ID")
	sample := fs.String("sample", "", "sample ID")
	candidate := fs.String("candidate", "", "candidate ID")
	finding := fs.String("finding", "", "finding ID")
	disposition := fs.String("disposition", "", "disposition: valid/invalid/unresolved")
	evidence := fs.String("evidence", "", "evidence for the decision")
	decidedBy := fs.String("decided-by", "", "actor recording the decision")
	if err := parseEvalFlags(fs, args, requireFlags(campaign, "--campaign", sample, "--sample",
		candidate, "--candidate", finding, "--finding", disposition, "--disposition",
		evidence, "--evidence", decidedBy, "--decided-by")); err != nil {
		return "", "", nil, err
	}
	body := map[string]interface{}{
		"candidate_id": *candidate, "finding_id": *finding, "disposition": *disposition,
		"evidence": *evidence, "decided_by": *decidedBy,
	}
	path := "/evaluation/campaigns/" + url.PathEscape(*campaign) + "/samples/" + url.PathEscape(*sample) + "/disposition"
	return http.MethodPost, path, body, nil
}

// evaluationResult is the result payload a runner may supply. The attempt
// identity and exit class are never taken from it, so --result cannot override
// the fence.
type evaluationResult struct {
	Status       *string         `json:"status,omitempty"`
	ErrorClass   *string         `json:"error_class,omitempty"`
	ErrorMessage *string         `json:"error_message,omitempty"`
	DurationMs   *int            `json:"duration_ms,omitempty"`
	UsageTokens  *int            `json:"usage_tokens,omitempty"`
	Findings     json.RawMessage `json:"findings,omitempty"`
}

func evaluationFinalizeAttempt(fs *flag.FlagSet, args []string) (string, string, interface{}, error) {
	job := fs.String("job", "", "job ID")
	attempt := fs.String("attempt", "", "attempt ID")
	exitClass := fs.String("exit-class", "", "exit class (completed, failed, cancelled, unknown, timeout, unavailable_snapshot, unavailable_source, invalid_output, incomplete_output)")
	result := fs.String("result", "", "result JSON: status, error_class, error_message, duration_ms, usage_tokens, findings")
	resultFile := fs.String("result-file", "", "file holding the result JSON")
	if err := parseEvalFlags(fs, args, requireFlags(job, "--job", attempt, "--attempt", exitClass, "--exit-class")); err != nil {
		return "", "", nil, err
	}
	raw, err := readJSONInput(*result, *resultFile, "result")
	if err != nil {
		return "", "", nil, err
	}
	var res evaluationResult
	if raw != nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&res); err != nil {
			return "", "", nil, fmt.Errorf("invalid --result: %w", err)
		}
	}
	body := struct {
		FenceAttemptID string `json:"fence_attempt_id"`
		ExitClass      string `json:"exit_class"`
		evaluationResult
	}{FenceAttemptID: *attempt, ExitClass: *exitClass, evaluationResult: res}
	return http.MethodPost, evaluationAttemptPath(*job, *attempt, "finalize"), body, nil
}
