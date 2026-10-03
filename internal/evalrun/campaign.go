package evalrun

import (
	"context"
	"fmt"
)

// RunCampaign runs every sample of a campaign on every one of its candidates,
// one at a time and candidate by candidate, and returns one report per pair it
// reached. Each call runs each pair once; the store's attempt caps bound the
// total no matter how often it is called.
//
// A paused campaign or a cancelled context ends the whole run. A candidate
// whose pool, cap or registration blocks it is skipped for its remaining
// samples, so one stuck candidate neither stalls nor starves the others.
func (r *Runner) RunCampaign(ctx context.Context, campaignID string) ([]Report, error) {
	if _, err := r.cfg.Backend.GetEvaluationCampaign(ctx, campaignID); err != nil {
		return nil, fmt.Errorf("evalrun: campaign %s: %w", campaignID, err)
	}
	candidates, err := r.cfg.Backend.ListEvaluationCandidates(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("evalrun: list candidates: %w", err)
	}
	samples, err := r.cfg.Backend.ListEvaluationSamples(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("evalrun: list samples: %w", err)
	}

	var reports []Report
candidates:
	for _, cand := range candidates {
		for _, sample := range samples {
			rep := r.Run(ctx, Job{CampaignID: campaignID, SampleID: sample.ID, CandidateID: cand.ID})
			reports = append(reports, rep)
			if ctx.Err() != nil || rep.Kind == KindPaused {
				return reports, nil
			}
			switch rep.Kind {
			case KindCapacityExhausted, KindDeferred, KindPoolNotConfigured, KindNotRegistered, KindHostError:
				continue candidates
			}
		}
	}
	return reports, nil
}
