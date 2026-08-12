package proxy

import (
	"fmt"
	rand "math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/cache"
	"github.com/Min9-sec/EchoMusicKeeper/helper/internal/model"
)

func partialPath(key string) string {
	return "temp/" + key + ".part"
}

func parseContentRange(header string) (model.ByteRange, int64, error) {
	if !strings.HasPrefix(header, "bytes ") {
		return model.ByteRange{}, 0, fmt.Errorf("invalid Content-Range")
	}
	bounds, totalText, ok := strings.Cut(strings.TrimPrefix(header, "bytes "), "/")
	if !ok || totalText == "" || strings.Contains(totalText, "/") {
		return model.ByteRange{}, 0, fmt.Errorf("invalid Content-Range")
	}
	startText, endText, ok := strings.Cut(bounds, "-")
	if !ok || strings.Contains(endText, "-") {
		return model.ByteRange{}, 0, fmt.Errorf("invalid Content-Range")
	}
	start, startErr := parseASCIIInt64(startText)
	end, endErr := parseASCIIInt64(endText)
	total, totalErr := parseASCIIInt64(totalText)
	if startErr != nil || endErr != nil || totalErr != nil || start < 0 || end < start || total <= end {
		return model.ByteRange{}, 0, fmt.Errorf("invalid Content-Range")
	}
	return model.ByteRange{Start: start, End: end + 1}, total, nil
}

func parseASCIIInt64(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("empty decimal")
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, fmt.Errorf("invalid decimal")
		}
	}
	return strconv.ParseInt(value, 10, 64)
}

func sanitizeRanges(ranges []model.ByteRange, fileSize, total int64) []model.ByteRange {
	normalized := cache.Merge(ranges, model.ByteRange{})
	clamped := make([]model.ByteRange, 0, len(normalized))
	limit := fileSize
	if total > 0 {
		limit = min(limit, total)
	}
	for _, interval := range normalized {
		interval.End = min(interval.End, limit)
		if interval.End > interval.Start {
			clamped = cache.Merge(clamped, interval)
		}
	}
	return clamped
}

func coveredBytes(ranges []model.ByteRange) int64 {
	var total int64
	for _, interval := range cache.Merge(ranges, model.ByteRange{}) {
		total += interval.End - interval.Start
	}
	return total
}

func contiguousEnd(ranges []model.ByteRange, position int64) int64 {
	for _, interval := range ranges {
		if interval.Start <= position && position < interval.End {
			return interval.End
		}
		if interval.Start > position {
			break
		}
	}
	return position
}

func firstMissingPosition(ranges []model.ByteRange, start, end int64) (int64, bool) {
	cursor := start
	for _, interval := range ranges {
		if interval.End <= cursor {
			continue
		}
		if interval.Start > cursor {
			return cursor, true
		}
		cursor = max(cursor, interval.End)
		if cursor >= end {
			return 0, false
		}
	}
	if cursor < end {
		return cursor, true
	}
	return 0, false
}

func remainingDemands(demands []byteDemand, ranges []model.ByteRange) []byteDemand {
	remaining := make([]byteDemand, 0, len(demands))
	for _, demand := range demands {
		if contiguousEnd(ranges, demand.start) < demand.end {
			remaining = append(remaining, demand)
		}
	}
	return remaining
}

func demandInterval(demand byteDemand, missing []model.ByteRange) (model.ByteRange, bool) {
	for _, gap := range missing {
		start := max(gap.Start, demand.start)
		end := min(gap.End, demand.end)
		if end > start {
			return model.ByteRange{Start: start, End: end}, true
		}
	}
	return model.ByteRange{}, false
}

func jitter(delay time.Duration) time.Duration {
	spread := int64(delay / 5)
	if spread == 0 {
		return delay
	}
	offset := rand.Int64N(spread*2+1) - spread
	return delay + time.Duration(offset)
}

func sortTasks(tasks []model.Task) {
	sort.Slice(tasks, func(left, right int) bool {
		if tasks[left].UpdatedAt.Equal(tasks[right].UpdatedAt) {
			return tasks[left].ID < tasks[right].ID
		}
		return tasks[left].UpdatedAt.After(tasks[right].UpdatedAt)
	})
}
