package cache

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

// Merge normalizes half-open byte ranges and combines overlaps and adjacency.
func Merge(ranges []model.ByteRange, next model.ByteRange) []model.ByteRange {
	all := append(append([]model.ByteRange(nil), ranges...), next)
	for index := range all {
		if all[index].Start < 0 {
			all[index].Start = 0
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Start == all[j].Start {
			return all[i].End < all[j].End
		}
		return all[i].Start < all[j].Start
	})

	out := make([]model.ByteRange, 0, len(all))
	for _, current := range all {
		if current.End <= current.Start {
			continue
		}
		if len(out) == 0 || current.Start > out[len(out)-1].End {
			out = append(out, current)
			continue
		}
		if current.End > out[len(out)-1].End {
			out[len(out)-1].End = current.End
		}
	}
	return out
}

// Missing returns every gap not covered by ranges within [0, total).
func Missing(total int64, ranges []model.ByteRange) []model.ByteRange {
	if total <= 0 {
		return nil
	}
	normalized := Merge(ranges, model.ByteRange{})
	missing := make([]model.ByteRange, 0, len(normalized)+1)
	cursor := int64(0)
	for _, current := range normalized {
		if current.Start >= total {
			break
		}
		if current.Start > cursor {
			missing = append(missing, model.ByteRange{Start: cursor, End: current.Start})
		}
		if current.End > cursor {
			cursor = min(current.End, total)
		}
	}
	if cursor < total {
		missing = append(missing, model.ByteRange{Start: cursor, End: total})
	}
	return missing
}

// ParseSingleRange parses one RFC 7233 bytes range into half-open bounds.
func ParseSingleRange(header string, total int64) (model.ByteRange, error) {
	if total <= 0 || !strings.HasPrefix(header, "bytes=") {
		return model.ByteRange{}, fmt.Errorf("invalid byte range")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if spec == "" || strings.Contains(spec, ",") || strings.Count(spec, "-") != 1 {
		return model.ByteRange{}, fmt.Errorf("multiple or invalid ranges are unsupported")
	}
	startText, endText, _ := strings.Cut(spec, "-")
	if startText == "" {
		suffix, err := parseRangeNumber(endText)
		if err != nil || suffix <= 0 {
			return model.ByteRange{}, fmt.Errorf("invalid suffix range")
		}
		suffix = min(suffix, total)
		return model.ByteRange{Start: total - suffix, End: total}, nil
	}

	start, err := parseRangeNumber(startText)
	if err != nil || start >= total {
		return model.ByteRange{}, fmt.Errorf("unsatisfiable byte range")
	}
	end := total
	if endText != "" {
		inclusiveEnd, parseErr := parseRangeNumber(endText)
		if parseErr != nil || inclusiveEnd < start {
			return model.ByteRange{}, fmt.Errorf("invalid byte range end")
		}
		if inclusiveEnd < total-1 {
			end = inclusiveEnd + 1
		}
	}
	return model.ByteRange{Start: start, End: end}, nil
}

func parseRangeNumber(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("empty byte range number")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("invalid byte range number")
		}
	}
	return strconv.ParseInt(value, 10, 64)
}
