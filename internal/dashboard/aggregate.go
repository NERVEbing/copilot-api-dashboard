package dashboard

import (
	"sort"

	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

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
		dst = append(dst, Day{Date: src.Date, Models: []upstream.Model{}})
		index = len(dst) - 1
	}
	addTotals(&dst[index].Totals, *src.Totals)
	dst[index].Models = mergeModels(dst[index].Models, src.Models)
	sort.Slice(dst, func(i, j int) bool { return dst[i].Date < dst[j].Date })
	return dst
}
