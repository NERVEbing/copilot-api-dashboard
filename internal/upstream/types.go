package upstream

type Quota struct {
	Entitlement      *float64 `json:"entitlement"`
	OverageCount     *float64 `json:"overage_count"`
	OveragePermitted *bool    `json:"overage_permitted"`
	PercentRemaining *float64 `json:"percent_remaining"`
	QuotaID          string   `json:"quota_id"`
	QuotaRemaining   *float64 `json:"quota_remaining"`
	Remaining        *float64 `json:"remaining"`
	Unlimited        *bool    `json:"unlimited"`
}

type Quotas struct {
	Chat                *Quota `json:"chat"`
	Completions         *Quota `json:"completions"`
	PremiumInteractions *Quota `json:"premium_interactions"`
}

type Usage struct {
	Login     string  `json:"login"`
	Plan      *string `json:"copilot_plan"`
	ResetDate *string `json:"quota_reset_date"`
	Quotas    *Quotas `json:"quota_snapshots"`
}

type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Nanos    int64   `json:"total_cost_nanos"`
}

type Totals struct {
	Input         int64  `json:"input_tokens"`
	Output        int64  `json:"output_tokens"`
	CacheRead     int64  `json:"cache_read_input_tokens"`
	CacheCreation int64  `json:"cache_creation_input_tokens"`
	Requests      int64  `json:"request_count"`
	Tokens        int64  `json:"total_tokens"`
	NanoAIU       *int64 `json:"total_nano_aiu"`
	Costs         []Cost `json:"costs"`
}

type Model struct {
	Model string `json:"model"`
	Totals
}

type Range struct {
	StartMS  int64  `json:"start_ms"`
	EndMS    int64  `json:"end_ms"`
	StartUTC string `json:"start_utc"`
	EndUTC   string `json:"end_utc"`
}

type Summary struct {
	Period string  `json:"period"`
	Range  Range   `json:"range"`
	Totals *Totals `json:"totals"`
	Models []Model `json:"byModel"`
}

type Day struct {
	Date    string  `json:"date"`
	StartMS int64   `json:"start_ms"`
	EndMS   int64   `json:"end_ms"`
	Totals  *Totals `json:"totals"`
	Models  []Model `json:"byModel"`
}

type Daily struct {
	Summary
	Days []Day `json:"days"`
}

type EventCost struct {
	Cost
	Source string `json:"source"`
}

type Event struct {
	ID            int64      `json:"id"`
	CreatedMS     int64      `json:"created_at_ms"`
	CreatedUTC    string     `json:"created_at_utc"`
	Endpoint      string     `json:"endpoint"`
	Model         string     `json:"model"`
	Provider      *string    `json:"provider_name"`
	SessionID     string     `json:"session_id"`
	Source        string     `json:"source"`
	TraceID       string     `json:"trace_id"`
	UserID        string     `json:"user_id"`
	Input         int64      `json:"input_tokens"`
	Output        int64      `json:"output_tokens"`
	CacheRead     int64      `json:"cache_read_input_tokens"`
	CacheCreation int64      `json:"cache_creation_input_tokens"`
	Tokens        int64      `json:"total_tokens"`
	NanoAIU       *int64     `json:"total_nano_aiu"`
	Cost          *EventCost `json:"cost"`
}

type Events struct {
	Items      []Event `json:"items"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	Total      int64   `json:"total"`
	TotalPages int     `json:"total_pages"`
	Period     string  `json:"period"`
	Range      Range   `json:"range"`
}

func ValidPeriod(p string) bool {
	switch p {
	case "today", "this_week", "last_7_days", "this_month", "last_30_days", "lifetime":
		return true
	}
	return false
}
