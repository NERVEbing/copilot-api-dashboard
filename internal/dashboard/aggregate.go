package dashboard

import (
	"sort"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

const maxFilledLifetimeDays = 366

func addTotals(dst *upstream.Totals, src upstream.Totals) {
	dst.Input += src.Input
	dst.Output += src.Output
	dst.CacheRead += src.CacheRead
	dst.CacheCreation += src.CacheCreation
	dst.Requests += src.Requests
	dst.Tokens += src.Tokens
	if src.NanoAIU != nil {
		if dst.NanoAIU == nil {
			dst.NanoAIU = new(int64)
		}
		*dst.NanoAIU += *src.NanoAIU
	}
	if dst.Costs == nil {
		dst.Costs = []upstream.Cost{}
	}
	for _, cost := range src.Costs {
		index := -1
		for i := range dst.Costs {
			if dst.Costs[i].Currency == cost.Currency {
				index = i
				break
			}
		}
		if index < 0 {
			dst.Costs = append(dst.Costs, upstream.Cost{Currency: cost.Currency})
			index = len(dst.Costs) - 1
		}
		dst.Costs[index].Nanos += cost.Nanos
		dst.Costs[index].Amount = float64(dst.Costs[index].Nanos) / 1e9
	}
	sort.Slice(dst.Costs, func(i, j int) bool { return dst.Costs[i].Currency < dst.Costs[j].Currency })
}

func mergeModels(dst, src []upstream.Model) []upstream.Model {
	for _, model := range src {
		index := -1
		for i := range dst {
			if dst[i].Model == model.Model {
				index = i
				break
			}
		}
		if index < 0 {
			dst = append(dst, upstream.Model{Model: model.Model})
			index = len(dst) - 1
		}
		addTotals(&dst[index].Totals, model.Totals)
	}
	sort.Slice(dst, func(i, j int) bool { return dst[i].Model < dst[j].Model })
	return dst
}

func mergeDay(dst []Day, src upstream.Day) []Day {
	index := -1
	for i := range dst {
		if dst[i].Date == src.Date {
			index = i
			break
		}
	}
	if index < 0 {
		dst = append(dst, Day{Date: src.Date, Recorded: true, Models: []upstream.Model{}})
		index = len(dst) - 1
	}
	dst[index].Recorded = true
	addTotals(&dst[index].Totals, *src.Totals)
	dst[index].Models = mergeModels(dst[index].Models, src.Models)
	sort.Slice(dst, func(i, j int) bool { return dst[i].Date < dst[j].Date })
	return dst
}

func fillDays(days []Day, period string, now time.Time) []Day {
	if days == nil || period == "today" || period == "lifetime" && len(days) == 0 {
		return days
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := today
	switch period {
	case "this_week":
		start = start.AddDate(0, 0, -int((start.Weekday()+6)%7))
	case "last_7_days":
		start = start.AddDate(0, 0, -6)
	case "this_month":
		start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location())
	case "last_30_days":
		start = start.AddDate(0, 0, -29)
	case "lifetime":
		start = start.AddDate(0, 0, -6)
		for _, day := range days {
			date, err := time.ParseInLocation("2006-01-02", day.Date, now.Location())
			if err != nil {
				return days
			}
			if date.Before(start) {
				start = date
			}
		}
		if start.AddDate(0, 0, maxFilledLifetimeDays-1).Before(today) {
			return days
		}
	default:
		return days
	}
	byDate := make(map[string]bool, len(days))
	for _, day := range days {
		byDate[day.Date] = true
	}
	for date := start; !date.After(today); date = date.AddDate(0, 0, 1) {
		key := date.Format("2006-01-02")
		if !byDate[key] {
			days = append(days, Day{Date: key, Totals: upstream.Totals{Costs: []upstream.Cost{}}, Models: []upstream.Model{}})
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
	return days
}
