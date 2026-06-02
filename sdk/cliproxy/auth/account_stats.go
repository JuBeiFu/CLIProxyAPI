package auth

import (
	"strings"
	"time"
)

// LoadBanRecordsRange loads ban records for every local day in [start, end] (inclusive).
func LoadBanRecordsRange(authDir string, start, end time.Time) ([]BanRecord, error) {
	out := make([]BanRecord, 0)
	for _, day := range dayRange(start, end) {
		recs, err := LoadBanRecordsForDay(authDir, day)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

// LoadAddRecordsRange loads add records for every local day in [start, end] (inclusive).
func LoadAddRecordsRange(authDir string, start, end time.Time) ([]AddRecord, error) {
	out := make([]AddRecord, 0)
	for _, day := range dayRange(start, end) {
		recs, err := LoadAddRecordsForDay(authDir, day)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

func dayRange(start, end time.Time) []time.Time {
	if end.Before(start) {
		start, end = end, start
	}
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, end.Location())
	days := make([]time.Time, 0)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
		if len(days) >= 400 { // safety cap (~13 months)
			break
		}
	}
	return days
}

// ClassifyBanReason buckets a freeform ban reason into a small, stable set so the
// dashboard can chart 封号原因分类. Order matters: more specific tokens first.
func ClassifyBanReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case r == "":
		return "unknown"
	case strings.Contains(r, "deactivat"):
		return "account_deactivated"
	case strings.Contains(r, "phone") || strings.Contains(r, "add-phone"):
		return "phone_required"
	case strings.Contains(r, "unsupported") || strings.Contains(r, "region") || strings.Contains(r, "country"):
		return "unsupported_region"
	case strings.Contains(r, "unauthorized") || strings.Contains(r, "401"):
		return "unauthorized"
	case strings.Contains(r, "invalid_grant") || strings.Contains(r, "revok") || strings.Contains(r, "refresh"):
		return "token_revoked"
	case strings.Contains(r, "403") || strings.Contains(r, "forbidden"):
		return "forbidden"
	case strings.Contains(r, "quota") || strings.Contains(r, "rate_limit") || strings.Contains(r, "rate limit"):
		return "quota_or_rate"
	default:
		return "other"
	}
}

// DailyStat is one day's added-vs-banned pair.
type DailyStat struct {
	Date   string `json:"date"`
	Added  int    `json:"added"`
	Banned int    `json:"banned"`
}

// AccountStats is the aggregate served to the ban-log dashboard.
type AccountStats struct {
	Start       string         `json:"start"`
	End         string         `json:"end"`
	Daily       []DailyStat    `json:"daily"`
	TotalAdded  int            `json:"total_added"`
	TotalBanned int            `json:"total_banned"`
	BanByPlan   map[string]int `json:"ban_by_plan"`
	BanByReason map[string]int `json:"ban_by_reason"`
	BanBySource map[string]int `json:"ban_by_source"`
	AddByPlan   map[string]int `json:"add_by_plan"`
}

// BuildAccountStats aggregates add/ban records over [start, end] (inclusive),
// bucketing each record by its timestamp's day in loc. Days with no events are
// still emitted (Added=Banned=0) so the chart has a continuous x-axis.
func BuildAccountStats(add []AddRecord, ban []BanRecord, start, end time.Time, loc *time.Location) AccountStats {
	if loc == nil {
		loc = time.Local
	}
	days := dayRange(start.In(loc), end.In(loc))
	addByDay := make(map[string]int, len(days))
	banByDay := make(map[string]int, len(days))
	st := AccountStats{
		Daily:       make([]DailyStat, 0, len(days)),
		BanByPlan:   map[string]int{},
		BanByReason: map[string]int{},
		BanBySource: map[string]int{},
		AddByPlan:   map[string]int{},
	}
	for _, rec := range add {
		key := rec.AddedAt.In(loc).Format("2006-01-02")
		addByDay[key]++
		st.TotalAdded++
		st.AddByPlan[planKey(rec.Plan)]++
	}
	for _, rec := range ban {
		key := rec.BannedAt.In(loc).Format("2006-01-02")
		banByDay[key]++
		st.TotalBanned++
		st.BanByPlan[planKey(rec.Plan)]++
		st.BanByReason[ClassifyBanReason(rec.Reason)]++
		st.BanBySource[sourceKey(rec.Source)]++
	}
	for _, d := range days {
		key := d.Format("2006-01-02")
		st.Daily = append(st.Daily, DailyStat{Date: key, Added: addByDay[key], Banned: banByDay[key]})
	}
	if len(days) > 0 {
		st.Start = days[0].Format("2006-01-02")
		st.End = days[len(days)-1].Format("2006-01-02")
	}
	return st
}

func planKey(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "unknown"
	}
	return p
}

func sourceKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "request"
	}
	return s
}
