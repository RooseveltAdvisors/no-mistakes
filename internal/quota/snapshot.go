// Package quota parses quota-axi snapshots and ranks subscription-backed
// agent candidates by known effective headroom and pace.
//
// Selection never invents healthy quota for unknown or stale providers, and
// never introduces providers that were not named in the candidate set.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// Snapshot is the normalized subset of quota-axi --json used for routing.
type Snapshot struct {
	GeneratedAt   string             `json:"generatedAt"`
	SchemaVersion int                `json:"schemaVersion"`
	Providers     []ProviderSnapshot `json:"providers"`
}

// ProviderSnapshot is one provider row from quota-axi.
type ProviderSnapshot struct {
	Provider       string         `json:"provider"`
	State          ProviderState  `json:"state"`
	Windows        []QuotaWindow  `json:"windows,omitempty"`
	QuotaSemantics QuotaSemantics `json:"quotaSemantics"`
}

type QuotaWindow struct {
	ID               string   `json:"id"`
	PercentRemaining *float64 `json:"percentRemaining"`
}

// ProviderState is the freshness / auth state of a provider report.
type ProviderState struct {
	Status string `json:"status"`
	Stale  bool   `json:"stale"`
	Error  string `json:"error,omitempty"`
}

// QuotaSemantics holds effective availability derived by quota-axi.
type QuotaSemantics struct {
	Status                string                  `json:"status"`
	EffectiveAvailability []EffectiveAvailability `json:"effectiveAvailability"`
}

// EffectiveAvailability is one scope's known headroom summary.
type EffectiveAvailability struct {
	Scope                     string   `json:"scope"`
	Status                    string   `json:"status"`
	EffectivePercentRemaining *float64 `json:"effectivePercentRemaining"`
	Pace                      *Pace    `json:"pace"`
}

// Pace is the effective pace summary for a scope.
type Pace struct {
	Status string `json:"status"`
}

// Candidate is one operator-named subscription-backed routing target.
type Candidate struct {
	// Name is the unique operator label (for example "kimi").
	Name string
	// Agent is the native/ACP backend used to run work (pi, codex, ...).
	Agent types.AgentName
	// QuotaProvider is the quota-axi provider id (kimi, codex, grok, ...).
	QuotaProvider string
	// Args are the effective extra CLI args for this candidate.
	Args []string
	// Model is an optional model hint used to pick a model-scoped availability row.
	Model string
	// Runnable is set by the caller after binary probing.
	Runnable bool
	// Probe explains why Runnable is false, or the binary that was probed.
	Probe string
}

// Decision is the inspectable ranking outcome for one candidate.
type Decision struct {
	Name          string
	Agent         types.AgentName
	QuotaProvider string
	Args          []string
	Eligible      bool
	// RankGroup orders eligibility bands: lower is preferred.
	// 0 = known fresh headroom > 0, 1 = known exhausted (0%), 2 = unknown/stale,
	// 3 = not runnable / missing config.
	RankGroup int
	Remaining *float64
	Pace      string
	Reason    string
}

// Selection is the ordered route produced from one fresh snapshot.
type Selection struct {
	// Ordered is the runnable candidates in preference order. Non-runnable
	// candidates are omitted so callers never launch them.
	Ordered []Decision
	// Decisions includes every input candidate with an inspectable reason.
	Decisions []Decision
	// Summary is a single-line operator-facing explanation.
	Summary string
}

// ParseSnapshot decodes a quota-axi --json payload.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse quota-axi json: %w", err)
	}
	if snap.SchemaVersion != 0 && snap.SchemaVersion != 2 && snap.SchemaVersion != 3 {
		return nil, fmt.Errorf("unsupported quota-axi schemaVersion %d (want 2 or 3)", snap.SchemaVersion)
	}
	if snap.SchemaVersion == 2 {
		normalizeV2(&snap)
	}
	return &snap, nil
}

func normalizeV2(snap *Snapshot) {
	for i := range snap.Providers {
		provider := &snap.Providers[i]
		availability := make([]EffectiveAvailability, 0, len(provider.Windows)+1)
		var minimum *float64
		for _, window := range provider.Windows {
			if window.PercentRemaining == nil {
				continue
			}
			value := *window.PercentRemaining
			availability = append(availability, EffectiveAvailability{
				Scope: window.ID, Status: "known",
				EffectivePercentRemaining: quotaFloatPtr(value),
				Pace:                      &Pace{Status: "unknown"},
			})
			if minimum == nil || value < *minimum {
				minimum = quotaFloatPtr(value)
			}
		}
		if minimum == nil {
			provider.QuotaSemantics = QuotaSemantics{Status: "unknown"}
			continue
		}
		availability = append([]EffectiveAvailability{{
			Scope: "all_models", Status: "known",
			EffectivePercentRemaining: minimum,
			Pace:                      &Pace{Status: "unknown"},
		}}, availability...)
		provider.QuotaSemantics = QuotaSemantics{Status: "known", EffectiveAvailability: availability}
	}
}

func quotaFloatPtr(value float64) *float64 {
	return &value
}

// FetchFunc loads one fresh quota-axi snapshot. Tests inject fakes.
type FetchFunc func(ctx context.Context, bin string, providers []string) (*Snapshot, error)

// DefaultFetch runs `quota-axi --json` (optionally scoped with --provider).
func DefaultFetch(ctx context.Context, bin string, providers []string) (*Snapshot, error) {
	if strings.TrimSpace(bin) == "" {
		bin = "quota-axi"
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	args := []string{"--json"}
	if len(providers) > 0 {
		args = append(args, "--provider", strings.Join(providers, ","))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr != "" {
			return nil, fmt.Errorf("run %s: %w: %s", bin, err, stderr)
		}
		return nil, fmt.Errorf("run %s: %w", bin, err)
	}
	return ParseSnapshot(out)
}

// Select ranks candidates using one snapshot. Unknown and stale quota are never
// treated as healthy headroom; config order breaks remaining ties.
//
// snap may be nil when the quota tool failed: every runnable candidate is then
// ranked in the unknown band with an explicit reason, preserving safe launch
// without inventing healthy percentages.
func Select(candidates []Candidate, snap *Snapshot, snapshotErr error) Selection {
	byProvider := map[string]ProviderSnapshot{}
	if snap != nil {
		for _, p := range snap.Providers {
			byProvider[strings.ToLower(strings.TrimSpace(p.Provider))] = p
		}
	}

	decisions := make([]Decision, 0, len(candidates))
	for i, c := range candidates {
		d := Decision{
			Name:          c.Name,
			Agent:         c.Agent,
			QuotaProvider: c.QuotaProvider,
			Args:          append([]string(nil), c.Args...),
		}
		if !c.Runnable {
			d.RankGroup = 3
			d.Eligible = false
			if c.Probe != "" {
				d.Reason = fmt.Sprintf("not runnable (%s)", c.Probe)
			} else {
				d.Reason = "not runnable"
			}
			decisions = append(decisions, d)
			continue
		}
		if snapshotErr != nil {
			d.RankGroup = 2
			d.Eligible = true
			d.Pace = "unknown"
			d.Reason = fmt.Sprintf("runnable; quota snapshot unavailable (%v); config order %d", snapshotErr, i)
			decisions = append(decisions, d)
			continue
		}
		if snap == nil {
			d.RankGroup = 2
			d.Eligible = true
			d.Pace = "unknown"
			d.Reason = fmt.Sprintf("runnable; quota snapshot missing; config order %d", i)
			decisions = append(decisions, d)
			continue
		}
		prov, ok := byProvider[strings.ToLower(strings.TrimSpace(c.QuotaProvider))]
		if !ok {
			d.RankGroup = 2
			d.Eligible = true
			d.Pace = "unknown"
			d.Reason = fmt.Sprintf("runnable; quota provider %q absent from snapshot; config order %d", c.QuotaProvider, i)
			decisions = append(decisions, d)
			continue
		}
		rankKnownAvailability(c, prov, i, &d)
		decisions = append(decisions, d)
	}

	// Stable sort: rank group, remaining desc, pace rank, original index.
	// Preserve original index via a parallel key.
	type scored struct {
		d     Decision
		index int
	}
	scoredList := make([]scored, len(decisions))
	for i, d := range decisions {
		scoredList[i] = scored{d: d, index: i}
	}
	sort.SliceStable(scoredList, func(i, j int) bool {
		a, b := scoredList[i], scoredList[j]
		if a.d.RankGroup != b.d.RankGroup {
			return a.d.RankGroup < b.d.RankGroup
		}
		ar := remainingOrNegInf(a.d.Remaining)
		br := remainingOrNegInf(b.d.Remaining)
		if ar != br {
			return ar > br
		}
		ap := paceRank(a.d.Pace)
		bp := paceRank(b.d.Pace)
		if ap != bp {
			return ap < bp
		}
		return a.index < b.index
	})

	ordered := make([]Decision, 0, len(scoredList))
	outDecisions := make([]Decision, 0, len(scoredList))
	for _, s := range scoredList {
		outDecisions = append(outDecisions, s.d)
		if s.d.Eligible && s.d.RankGroup < 3 {
			ordered = append(ordered, s.d)
		}
	}

	summary := summarize(ordered, snapshotErr)
	return Selection{Ordered: ordered, Decisions: outDecisions, Summary: summary}
}

func rankKnownAvailability(c Candidate, prov ProviderSnapshot, index int, d *Decision) {
	state := strings.ToLower(strings.TrimSpace(prov.State.Status))
	if prov.State.Stale || state == "stale" {
		d.RankGroup = 2
		d.Eligible = true
		d.Pace = "unknown"
		d.Reason = fmt.Sprintf("runnable; quota stale; not treated as healthy; config order %d", index)
		return
	}
	if state == "rate_limited" {
		d.RankGroup = 2
		d.Eligible = true
		d.Pace = "unknown"
		errText := strings.TrimSpace(prov.State.Error)
		if errText == "" {
			errText = "rate limited"
		}
		d.Reason = fmt.Sprintf("runnable; quota rate_limited (%s); not treated as healthy; config order %d", errText, index)
		return
	}
	if state != "" && state != "fresh" {
		d.RankGroup = 2
		d.Eligible = true
		d.Pace = "unknown"
		d.Reason = fmt.Sprintf("runnable; quota state %q; not treated as healthy; config order %d", state, index)
		return
	}
	if strings.ToLower(strings.TrimSpace(prov.QuotaSemantics.Status)) != "known" {
		d.RankGroup = 2
		d.Eligible = true
		d.Pace = "unknown"
		sem := strings.TrimSpace(prov.QuotaSemantics.Status)
		if sem == "" {
			sem = "unknown"
		}
		d.Reason = fmt.Sprintf("runnable; quota semantics %q; not treated as healthy; config order %d", sem, index)
		return
	}
	avail := pickAvailability(prov.QuotaSemantics.EffectiveAvailability, c.Model)
	if avail == nil || strings.ToLower(strings.TrimSpace(avail.Status)) != "known" || avail.EffectivePercentRemaining == nil {
		d.RankGroup = 2
		d.Eligible = true
		d.Pace = "unknown"
		d.Reason = fmt.Sprintf("runnable; no known effective headroom in snapshot; config order %d", index)
		return
	}
	remaining := *avail.EffectivePercentRemaining
	d.Remaining = avail.EffectivePercentRemaining
	pace := "unknown"
	if avail.Pace != nil && strings.TrimSpace(avail.Pace.Status) != "" {
		pace = strings.ToLower(strings.TrimSpace(avail.Pace.Status))
	}
	d.Pace = pace
	d.Eligible = true
	if remaining <= 0 {
		d.RankGroup = 1
		d.Reason = fmt.Sprintf("runnable; known exhausted headroom 0%% (scope %s, pace %s); config order %d", avail.Scope, pace, index)
		return
	}
	d.RankGroup = 0
	d.Reason = fmt.Sprintf("runnable; known headroom %.4g%% (scope %s, pace %s); config order %d", remaining, avail.Scope, pace, index)
}

func pickAvailability(avails []EffectiveAvailability, model string) *EffectiveAvailability {
	if len(avails) == 0 {
		return nil
	}
	model = strings.TrimSpace(model)
	if model != "" {
		want := "model:" + model
		for i := range avails {
			if avails[i].Scope == want {
				return &avails[i]
			}
		}
		for i := range avails {
			if strings.Contains(avails[i].Scope, model) {
				return &avails[i]
			}
		}
	}
	for _, prefer := range []string{"all_models", "all_products"} {
		for i := range avails {
			if avails[i].Scope == prefer {
				return &avails[i]
			}
		}
	}
	for i := range avails {
		if strings.ToLower(strings.TrimSpace(avails[i].Status)) == "known" && avails[i].EffectivePercentRemaining != nil {
			return &avails[i]
		}
	}
	return &avails[0]
}

func remainingOrNegInf(v *float64) float64 {
	if v == nil {
		return math.Inf(-1)
	}
	return *v
}

func paceRank(pace string) int {
	switch strings.ToLower(strings.TrimSpace(pace)) {
	case "behind":
		return 0
	case "on_pace":
		return 1
	case "mixed":
		return 2
	case "ahead":
		return 3
	default:
		return 4
	}
}

func summarize(ordered []Decision, snapshotErr error) string {
	if len(ordered) == 0 {
		if snapshotErr != nil {
			return fmt.Sprintf("no runnable subscription candidates (quota snapshot error: %v)", snapshotErr)
		}
		return "no runnable subscription candidates"
	}
	parts := make([]string, 0, len(ordered))
	for _, d := range ordered {
		label := d.Name
		if d.Remaining != nil {
			label = fmt.Sprintf("%s(%.4g%%,%s)", d.Name, *d.Remaining, d.Pace)
		} else {
			label = fmt.Sprintf("%s(%s)", d.Name, d.Pace)
		}
		parts = append(parts, label)
	}
	prefix := "quota route"
	if snapshotErr != nil {
		prefix = "quota route (snapshot unavailable; config order)"
	}
	return prefix + ": " + strings.Join(parts, " -> ")
}
