package auth

import (
	"testing"
	"time"
)

func TestClassifyBanReason(t *testing.T) {
	cases := map[string]string{
		"account_deactivated: login rejected at authorize": "account_deactivated",
		`401 {"detail":"Unauthorized"}`:                     "unauthorized",
		"add-phone required":                               "phone_required",
		"unsupported_country_region":                       "unsupported_region",
		"invalid_grant":                                    "token_revoked",
		"403 forbidden":                                    "forbidden",
		"":                                                 "unknown",
		"weird thing":                                      "other",
	}
	for in, want := range cases {
		if got := ClassifyBanReason(in); got != want {
			t.Errorf("ClassifyBanReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildAccountStats(t *testing.T) {
	loc := time.UTC
	d := func(s string) time.Time { tm, _ := time.ParseInLocation("2006-01-02", s, loc); return tm }
	add := []AddRecord{
		{AddedAt: d("2026-06-01").Add(2 * time.Hour), Plan: "free"},
		{AddedAt: d("2026-06-01").Add(5 * time.Hour), Plan: "free"},
		{AddedAt: d("2026-06-02").Add(1 * time.Hour), Plan: "plus"},
	}
	ban := []BanRecord{
		{BannedAt: d("2026-06-02").Add(3 * time.Hour), Plan: "free", Reason: "account_deactivated: x", Source: "request"},
		{BannedAt: d("2026-06-02").Add(4 * time.Hour), Plan: "plus", Reason: "401 unauthorized", Source: "background"},
	}
	st := BuildAccountStats(add, ban, d("2026-06-01"), d("2026-06-03"), loc)

	if st.TotalAdded != 3 || st.TotalBanned != 2 {
		t.Fatalf("totals add=%d ban=%d", st.TotalAdded, st.TotalBanned)
	}
	if len(st.Daily) != 3 {
		t.Fatalf("daily len=%d want 3 (continuous x-axis)", len(st.Daily))
	}
	if st.Daily[0].Date != "2026-06-01" || st.Daily[0].Added != 2 || st.Daily[0].Banned != 0 {
		t.Errorf("day0=%+v", st.Daily[0])
	}
	if st.Daily[1].Date != "2026-06-02" || st.Daily[1].Added != 1 || st.Daily[1].Banned != 2 {
		t.Errorf("day1=%+v", st.Daily[1])
	}
	if st.Daily[2].Added != 0 || st.Daily[2].Banned != 0 {
		t.Errorf("day2 should be an empty filler day: %+v", st.Daily[2])
	}
	if st.BanByPlan["free"] != 1 || st.BanByPlan["plus"] != 1 {
		t.Errorf("banByPlan=%v", st.BanByPlan)
	}
	if st.BanByReason["account_deactivated"] != 1 || st.BanByReason["unauthorized"] != 1 {
		t.Errorf("banByReason=%v", st.BanByReason)
	}
	if st.BanBySource["request"] != 1 || st.BanBySource["background"] != 1 {
		t.Errorf("banBySource=%v", st.BanBySource)
	}
	if st.AddByPlan["free"] != 2 || st.AddByPlan["plus"] != 1 {
		t.Errorf("addByPlan=%v", st.AddByPlan)
	}
}

func TestAddRecordRoundTripAndRange(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 6, 2, 10, 0, 0, 0, time.Local)
	a := &Auth{Provider: "codex", FileName: "codex-a@x-free.json",
		Attributes: map[string]string{"plan_type": "plus", "account": "a@x"}}
	b := &Auth{Provider: "codex", FileName: "codex-b@x-free.json",
		Attributes: map[string]string{"account": "b@x"}}

	if err := AppendAddRecord(dir, a, day1); err != nil {
		t.Fatal(err)
	}
	if err := AppendAddRecord(dir, b, day2); err != nil {
		t.Fatal(err)
	}

	recs1, err := LoadAddRecordsForDay(dir, day1)
	if err != nil || len(recs1) != 1 {
		t.Fatalf("day1 load err=%v n=%d", err, len(recs1))
	}
	if recs1[0].Account != "a@x" || recs1[0].Plan != "plus" || recs1[0].Name != "codex-a@x-free.json" {
		t.Errorf("rec=%+v", recs1[0])
	}

	all, err := LoadAddRecordsRange(dir, day1, day2)
	if err != nil || len(all) != 2 {
		t.Fatalf("range err=%v n=%d", err, len(all))
	}

	// A day with no file -> empty, not an error.
	empty, err := LoadAddRecordsForDay(dir, time.Date(2026, 5, 1, 0, 0, 0, 0, time.Local))
	if err != nil || len(empty) != 0 {
		t.Fatalf("missing day err=%v n=%d", err, len(empty))
	}
}
